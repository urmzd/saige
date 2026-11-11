package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/event"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

// Holding a run for the host.
//
// A run parks on the questions its agent asks (park.go). A host sometimes has
// a question of its own about a run: one that reached a spending limit may go
// on only if a person says so. Hold is how it asks.
//
// # The request is a record
//
// Hold writes the request on the run in the ledger, as the write-once event
// record "event:fiber.hold.<id>#0", after checking that the run exists, is the
// asker's tenant's and has not ended. Nothing is published: the turn reads
// the run's holds itself, from the ledger, every time the model sends a
// message with tool calls (turn.WithHolds), so a hold recorded while the run
// is queued, parked, between attempts or mid-turn is found at its next batch
// of tool calls, by whichever attempt executes it.
//
// # The wait is a question
//
// The turn parks that batch on a question of kind event.QuestionHold (see
// "Holding a turn for the host" in pkg/turn/native's park.go), and from there
// it is a question like any other: announced once on lifecycle.parked,
// answered through Answer, resolved write-once, expired by its deadline, and
// stopped by Stop. Only the decision differs. An approval continues the turn,
// which runs the batch it held; a denial, or nobody answering in time, ends
// the run stopped, with the answer's note as the reason, and the batch never
// runs. The body decides from the resolution record alone, so a replay
// decides the same way.
//
// A turn that ends without another batch of tool calls is never held: its
// last model call was already made, and there is nothing left to stop.

// holdRecordPrefix names a hold's record; the id follows.
const holdRecordPrefix = "fiber.hold."

func holdRecordName(id string) string { return holdRecordPrefix + id }
func holdRecordKey(id string) string  { return "event:" + holdRecordName(id) + "#0" }

// prefixHold is the key prefix of every hold record.
const prefixHold = "event:" + holdRecordPrefix

// MaxHoldID bounds a hold's id, in bytes.
const MaxHoldID = 128

// MaxHoldData bounds a hold's Data, in bytes. It travels on the question, in
// an event every sink is sent.
const MaxHoldData = 4 << 10

// ErrHoldUnavailable reports a runtime that cannot hold a run: it has no
// ledger to record the hold in (WithStopLedger).
var ErrHoldUnavailable = errors.New("runtime: holds are not available")

// ErrNotHeld reports a hold that was not recorded because there is no run to
// hold: none with that id, one of another tenant, or one that has ended. The
// three are one error on purpose, as for a question.
var ErrNotHeld = errors.New("runtime: no run to hold")

// HoldRequest asks that a run wait for a person before it does anything more.
type HoldRequest struct {
	// Tenant is the caller's, from its credential. A run of another tenant
	// is reported as ErrNotHeld.
	Tenant string
	// RunID is the run.
	RunID string
	// Hold is what to ask. Its ID is the caller's, unique within the run:
	// holding a run twice with one id records the first request and is not
	// an error, so a caller may repeat a request it is unsure was made.
	Hold turnrt.Hold
}

// Hold asks that a run wait for a person before it runs anything more, and
// returns once the request is recorded. The run parks at its next batch of
// tool calls on a question of kind event.QuestionHold carrying the hold; see
// "Holding a run for the host" in hold.go for what an answer does.
//
// It returns ErrInvalid for a hold with no id, an id longer than MaxHoldID or
// holding anything but letters, digits and ".:_-", or Data over MaxHoldData
// or not JSON; ErrNotHeld when there is no run of the tenant's, still going,
// to hold; and ErrHoldUnavailable without WithStopLedger.
func (r *Runtime) Hold(ctx context.Context, req HoldRequest) error {
	if r.stops == nil {
		return fmt.Errorf("%w: no ledger to record it in", ErrHoldUnavailable)
	}
	req.RunID = strings.TrimSpace(req.RunID)
	h := req.Hold
	h.ID = strings.TrimSpace(h.ID)
	switch {
	case req.RunID == "":
		return fmt.Errorf("%w: a hold names a run", ErrInvalid)
	case !validHoldID(h.ID):
		return fmt.Errorf("%w: a hold's id is 1 to %d letters, digits or \".:_-\"", ErrInvalid, MaxHoldID)
	case len(h.Data) > MaxHoldData:
		return fmt.Errorf("%w: a hold's data is at most %d bytes", ErrInvalid, MaxHoldData)
	case len(h.Data) > 0 && !json.Valid(h.Data):
		return fmt.Errorf("%w: a hold's data is JSON", ErrInvalid)
	}
	h.Prompt = clipQuestion(h.Prompt)
	h.Topic = clip(h.Topic)
	if h.ExpiresIn < 0 {
		h.ExpiresIn = 0
	}
	rec, err := r.loadRun(ctx, req.RunID)
	switch {
	case errors.Is(err, run.ErrNotFound):
		return ErrNotHeld
	case err != nil:
		return fmt.Errorf("runtime: hold %s: read the run: %w", req.RunID, err)
	}
	var in Input
	if err := json.Unmarshal(rec.Input, &in); err != nil || in.Trigger.Tenant != req.Tenant || rec.Status.Terminal() {
		return ErrNotHeld
	}
	body, err := json.Marshal(holdRecord{Hold: h, At: r.now().UTC()})
	if err != nil {
		return fmt.Errorf("runtime: hold %s: %w", req.RunID, err)
	}
	_, err = r.stops.Record(context.WithoutCancel(ctx), run.Record{
		RunID: req.RunID, Key: holdRecordKey(h.ID), Kind: run.KindEvent, Name: holdRecordName(h.ID),
		Codec: "application/json", Status: run.RecordOK, Output: body,
	})
	switch {
	case err == nil, errors.Is(err, run.ErrAlreadyRecorded):
		return nil
	case errors.Is(err, run.ErrNotFound):
		return ErrNotHeld
	default:
		return fmt.Errorf("runtime: hold %s: record the request: %w", req.RunID, err)
	}
}

// holdRecord is a hold as its record holds it.
type holdRecord struct {
	Hold turnrt.Hold `json:"hold"`
	At   time.Time   `json:"at"`
}

func validHoldID(id string) bool {
	if id == "" || len(id) > MaxHoldID {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == ':', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// holdsOf is how a turn of runID reads the run's holds: every hold recorded
// on it, oldest first. The turn skips those its answers settle. A ledger that
// cannot be read is logged and reads as no holds: the turn reads again at its
// next batch, and failing a run because a read failed would be worse.
func (r *Runtime) holdsOf(runID string) turnrt.Holds {
	return func(ctx context.Context) []turnrt.Hold {
		_, records, err := r.recordsOf(ctx, runID, false, prefixHold)
		if err != nil {
			r.logger.Warn("flow/runtime: could not read a run's holds; reading again at its next tool calls",
				"run_id", runID, "error", err)
			return nil
		}
		type held struct {
			h  turnrt.Hold
			at time.Time
		}
		var out []held
		for _, rec := range records {
			if rec.Kind != run.KindEvent || !strings.HasPrefix(rec.Name, holdRecordPrefix) || rec.Status != run.RecordOK {
				continue
			}
			var body holdRecord
			if json.Unmarshal(rec.Output, &body) != nil || body.Hold.ID != strings.TrimPrefix(rec.Name, holdRecordPrefix) {
				continue
			}
			out = append(out, held{body.Hold, body.At})
		}
		slices.SortStableFunc(out, func(a, b held) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return strings.Compare(a.h.ID, b.h.ID)
		})
		holds := make([]turnrt.Hold, len(out))
		for i, x := range out {
			holds[i] = x.h
		}
		return holds
	}
}

// heldStop is how a run whose hold was not approved ends: stopped, by whoever
// declined, with their note as the reason, or by nobody when it expired.
func heldStop(q event.Question, a event.Answer) native.Stop {
	reason := strings.TrimSpace(a.Note)
	switch {
	case a.Expired:
		reason = "Nobody answered in time, so it was stopped: " + q.Prompt
	case reason == "":
		reason = "A person declined to let it go on: " + q.Prompt
	}
	return native.Stop{Reason: clip(reason), By: a.By}
}
