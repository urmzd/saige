package anchor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	dt "github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// Record names of open runs.
const (
	inputEvent  = "input-" // the event carrying appended input segment k: "input-<k>"
	finalPrefix = "final:" // the step recording segment k's answer: "final:<k>"
)

// RegisterOption configures a registered workflow.
type RegisterOption func(*registered)

type registered struct{ open bool }

// Open keeps the workflow's runs open for more input: a run answers its
// first input, then waits, off the queue and at no cost, for Append (or a
// Start with longer input) to add a segment, and answers that in the same
// conversation. An open run never completes on its own; Cancel ends it.
func Open() RegisterOption { return func(r *registered) { r.open = true } }

// inputPayload is the recorded form of an appended segment. Messages is a
// durablecodec record.
type inputPayload struct {
	Key      string    `json:"key,omitempty"`
	At       time.Time `json:"at"`
	Messages []byte    `json:"messages"`
}

// segmentPrefix namespaces the step names and interrupt IDs of segment k.
// The first segment keeps unprefixed names.
func segmentPrefix(k int) string {
	if k == 0 {
		return ""
	}
	return fmt.Sprintf("input-%d/", k)
}

// executeOpen runs every input segment in order on one agent, recording
// each segment's answer, then parks until the next segment arrives.
func (e *Engine) executeOpen(ctx context.Context, a *agent.Agent, r *runner, first []types.Message) (runOutput, error) {
	msgs := first
	for k := 0; ; k++ {
		if k > 0 {
			p, err := dt.Event[inputPayload](ctx, inputEvent+strconv.Itoa(k))
			if err != nil {
				return runOutput{}, err // run.ErrParked: idle until Append
			}
			msgs = nil
			if err := decode(p.Messages, &msgs); err != nil {
				return runOutput{}, dt.NonRetryable(fmt.Errorf("decode input segment %d: %w", k, err))
			}
		}
		var step types.StepRunner = r
		if prefix := segmentPrefix(k); prefix != "" {
			step = segmentRunner{r: r, prefix: prefix}
		}
		final, err := a.RunDurable(ctx, step, msgs, "")
		if err != nil {
			return runOutput{}, outcome(ctx, err)
		}
		if _, err := dt.Step(ctx, finalPrefix+strconv.Itoa(k), func(context.Context) ([]byte, error) {
			if final == nil {
				return nil, nil
			}
			return encode(*final)
		}); err != nil {
			return runOutput{}, err
		}
	}
}

// segment is one recorded input segment of a run.
type segment struct {
	seq      int
	key      string
	at       time.Time
	messages []types.Message
}

// segments decodes the run's input log: its first input, then each appended
// segment in order.
func segments(r run.Run, records []run.Record) ([]segment, error) {
	var in runInput
	if err := json.Unmarshal(r.Input, &in); err != nil {
		return nil, fmt.Errorf("decode stored input: %w", err)
	}
	var first []types.Message
	if err := decode(in.Messages, &first); err != nil {
		return nil, fmt.Errorf("decode stored input: %w", err)
	}
	out := []segment{{seq: 0, at: r.CreatedAt, messages: first}}
	var more []segment
	for _, rec := range records {
		k, ok := inputSeq(rec)
		if !ok {
			continue
		}
		var p inputPayload
		if err := json.Unmarshal(rec.Output, &p); err != nil {
			return nil, fmt.Errorf("decode input segment %d: %w", k, err)
		}
		var msgs []types.Message
		if err := decode(p.Messages, &msgs); err != nil {
			return nil, fmt.Errorf("decode input segment %d: %w", k, err)
		}
		more = append(more, segment{seq: k, key: p.Key, at: p.At, messages: msgs})
	}
	sort.Slice(more, func(i, j int) bool { return more[i].seq < more[j].seq })
	return append(out, more...), nil
}

// inputSeq reports the segment number of an input event record.
func inputSeq(rec run.Record) (int, bool) {
	if rec.Kind != run.KindEvent || !strings.HasPrefix(rec.Name, inputEvent) || rec.Key != "event:"+rec.Name+"#0" {
		return 0, false
	}
	k, err := strconv.Atoi(strings.TrimPrefix(rec.Name, inputEvent))
	return k, err == nil && k > 0
}

// Append adds msgs to an open run's input log as a new segment, and wakes
// the run to answer it after the earlier segments. key makes the call
// idempotent: the same key with the same messages is a no-op, and with
// different messages ErrConflict. A finished run returns ErrClosed; a run of
// a workflow not registered with Open returns ErrConflict.
func (e *Engine) Append(ctx context.Context, id, key string, msgs []types.Message) error {
	if key == "" {
		return errors.New("append idempotency key required")
	}
	if len(msgs) == 0 {
		return errors.New("append requires at least one message")
	}
	raw, normalized, err := normalize(msgs)
	if err != nil {
		return err
	}
	if err := e.appendSegment(ctx, id, key, raw, normalized); err != nil {
		return err
	}
	return e.signal(ctx, Signal{RunID: id, Kind: SignalInput, ID: key})
}

// appendSegment records a segment and enqueues the run. A segment with the
// same non-empty key is matched instead of added again.
func (e *Engine) appendSegment(ctx context.Context, id, key string, raw []byte, normalized []types.Message) error {
	for {
		r, records, err := e.lgr.Load(ctx, id)
		if err != nil {
			return err
		}
		if r.Status.Terminal() {
			return ErrClosed
		}
		if !e.isOpen(r.Name) {
			return fmt.Errorf("%w: run %s is not an open run", ErrConflict, id)
		}
		segs, err := segments(r, records)
		if err != nil {
			return err
		}
		if key != "" {
			for _, s := range segs {
				if s.key != key {
					continue
				}
				if !reflect.DeepEqual(s.messages, normalized) {
					return fmt.Errorf("%w: append key %q reused with different messages", ErrConflict, key)
				}
				return nil
			}
		}
		k := len(segs)
		payload, err := json.Marshal(inputPayload{Key: key, At: time.Now().UTC(), Messages: raw})
		if err != nil {
			return err
		}
		name := inputEvent + strconv.Itoa(k)
		_, err = e.lgr.Record(ctx, run.Record{
			RunID: id, Key: "event:" + name + "#0", Kind: run.KindEvent, Name: name,
			Codec: run.JSONCodec{}.ContentType(), Status: run.RecordOK, Output: payload,
		})
		if errors.Is(err, run.ErrAlreadyRecorded) {
			continue // another append took segment k; look again
		}
		if err != nil {
			return err
		}
		if err := e.q.Enqueue(ctx, id, 0); err != nil {
			return fmt.Errorf("input is recorded; enqueue run %s: %w", id, err)
		}
		return nil
	}
}

// extend checks input against an open run's whole input log, message by
// message. A prefix of the log (nil included) changes nothing; extra
// messages past its end become a new segment.
func (e *Engine) extend(ctx context.Context, id string, normalized []types.Message) error {
	r, records, err := e.lgr.Load(ctx, id)
	if err != nil {
		return err
	}
	segs, err := segments(r, records)
	if err != nil {
		return err
	}
	i := 0
	for _, s := range segs {
		for _, logged := range s.messages {
			if i == len(normalized) {
				return nil
			}
			if !reflect.DeepEqual(logged, normalized[i]) {
				return fmt.Errorf("%w: input message %d differs from the input log", ErrConflict, i)
			}
			i++
		}
	}
	if i == len(normalized) {
		return nil
	}
	extra := normalized[i:]
	raw, _, err := normalize(extra)
	if err != nil {
		return err
	}
	return e.appendSegment(ctx, id, "", raw, extra)
}

// segmentRunner runs one later input segment against the run, namespacing
// its step names and interrupt IDs under the segment prefix. It forwards
// every optional runner capability the agent looks for.
type segmentRunner struct {
	r      *runner
	prefix string
}

var (
	_ types.StepRunner              = segmentRunner{}
	_ types.ApprovalRunner          = segmentRunner{}
	_ types.ConcurrentStepRunner    = segmentRunner{}
	_ types.SharedBudgetRunner      = segmentRunner{}
	_ types.BudgetReservationRunner = segmentRunner{}
	_ types.InterruptRouter         = segmentRunner{}
)

func (s segmentRunner) RunStep(ctx context.Context, name string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	return s.r.RunStep(ctx, s.prefix+name, fn)
}

func (s segmentRunner) ConcurrentSteps() bool  { return s.r.ConcurrentSteps() }
func (s segmentRunner) SharedBudgetOnly() bool { return s.r.SharedBudgetOnly() }

func (s segmentRunner) RecordReservation(ctx context.Context, name string, receipt types.BudgetReceipt) error {
	return s.r.RecordReservation(ctx, s.prefix+name, receipt)
}

func (s segmentRunner) ResolveApproval(ctx context.Context, req types.ApprovalRequest) (types.ApprovalDecision, error) {
	req.ID = s.prefix + req.ID
	return s.r.ResolveApproval(ctx, req)
}

// Post stores the interrupt under the prefixed ID and reports replies under
// the ID the caller posted.
func (s segmentRunner) Post(ctx context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	id := in.ID
	in.ID = s.prefix + id
	ch, err := s.r.Post(ctx, in)
	if err != nil {
		return nil, err
	}
	out := make(chan types.InterruptReply, 1)
	for reply := range ch {
		reply.ID = id
		out <- reply
	}
	close(out)
	return out, nil
}

// Reply and Pending use stored IDs, which are the IDs a host sees.
func (s segmentRunner) Reply(ctx context.Context, reply types.InterruptReply) error {
	return s.r.Reply(ctx, reply)
}

func (s segmentRunner) Pending(ctx context.Context, runID string) ([]types.Interrupt, error) {
	return s.r.Pending(ctx, runID)
}
