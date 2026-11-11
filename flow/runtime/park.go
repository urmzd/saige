package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/run"

	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/turn/native"
)

// Parking a run on a person.
//
// A turn can need a person: a tool call that needs approval, or the agent's
// ask_person tool (see pkg/turn/native's park.go for how a turn ends at the
// question and how a later turn continues it). This file makes that wait
// durable.
//
// # The wait is a durable event
//
// The agent activity returns a parked outcome: the question, and everything a
// continuation needs. Its result is memoized like any other. The workflow body
// then
//
//  1. announces the question once, as a durable step that publishes
//     lifecycle.parked with the question, so the step's record is the question
//     as the run asked it ("fiber.question.<id>#0"), and a replay neither
//     asks again nor announces again;
//  2. reads the question's resolution, a durable event record
//     ("event:fiber.answer.<id>#0"). With none, the body returns
//     run.ErrParked: the worker settles the run, it holds no goroutine and no
//     lease, it costs no retry budget, and it survives any restart, because
//     the question and the turn are in the ledger;
//  3. with one, continues the turn with the answer as the
//     fiber.turn.resume activity, whose input is the parked outcome and
//     the answer, both read from records.
//
// The resolution is write-once and the first write is the only one that
// counts, which is what makes an answer exactly-once: Answer writes it, a stop
// writes it, and an expiry writes it, and every one of them reads back
// whichever was first. The body decides only from that record, so a replay
// decides the same way whatever has been asked since.
//
// # Waking
//
// Answer and Stop enqueue the run once they have written the resolution
// (WithWaker), and the next attempt replays to the question and continues.
// Without a waker, or if the enqueue is lost, the worker's janitor re-enqueues
// every pending run on its own interval, and the run continues then.
//
// # Expiry
//
// A question has a deadline (WithQuestionTTL). Nothing wakes a run at that
// moment: the next attempt to find the question unanswered and past its
// deadline (the janitor's poll, or an answer that arrives late) writes the
// question's default as its resolution: a denial for an approval, the stated
// default for a choice or a reply, or "nobody answered", which the agent is
// told. Reading the clock only decides whether to write that record; what the
// run does is decided by the record, as for any answer. A question is never
// expired in the attempt that asked it, so a person always sees it first.
//
// # Stopping a parked run
//
// Stop records the request and then resolves the pending question as
// stopped, and wakes the run, which ends lifecycle.stopped without resuming
// its turn: nothing waits for a worker to pick a turn up that is not running.
// A stop recorded while the turn was still running, before it parked, is found
// the same way when the body reaches the question.

// Record names. A question is a durable step the body records; its
// resolution is an event record anything outside the run may write.
const (
	questionStepPrefix = "fiber.question."
	answerRecordPrefix = "fiber.answer."
	childStepPrefix    = "fiber.child."
)

func questionStepKey(id string) string  { return questionStepPrefix + id + "#0" }
func answerRecordName(id string) string { return answerRecordPrefix + id }
func answerRecordKey(id string) string  { return "event:" + answerRecordName(id) + "#0" }
func isQuestionRecord(rec run.Record) bool {
	return rec.Kind == run.KindStep && strings.HasPrefix(rec.Name, questionStepPrefix)
}

// Continue is the input of the activity that continues a parked turn.
type Continue struct {
	// Input is the run's pinned input.
	Input Input `json:"input"`
	// Seq numbers the continuation within the run, from 1.
	Seq int `json:"seq"`
	// Parked is the turn to continue, with the answers its pending calls
	// have, the new one included.
	Parked native.Parked `json:"parked"`
}

// resolution is how a question was resolved, as its event record holds it.
type resolution struct {
	QuestionID string        `json:"question_id"`
	Answer     *event.Answer `json:"answer,omitempty"`
	Stopped    *native.Stop  `json:"stopped,omitempty"`
}

// Waker enqueues a run so a worker replays it. Every durable queue
// implements it.
type Waker interface {
	Enqueue(ctx context.Context, runID string, delay time.Duration) error
}

// WithWaker wakes a parked run as soon as its question is answered or it is
// stopped. Pass the queue the runtime's durable client uses. Without it a
// parked run continues when the worker's janitor next polls it.
func WithWaker(w Waker) Option {
	return func(r *Runtime) { r.waker = w }
}

// WithQuestionTTL sets how long a question waits for an answer before its
// default applies (default native.DefaultQuestionTTL).
func WithQuestionTTL(d time.Duration) Option {
	return func(r *Runtime) {
		if d > 0 {
			r.questionTTL = d
		}
	}
}

// RecordReader reads the records of a run whose keys start with one of
// prefixes. pkg/durable's Records implements it for Postgres. It is how a poll
// that wants one small record (a stop request, a question's answer) avoids
// reading every record of the run, a parked turn's transcript among them.
type RecordReader interface {
	RecordsWithPrefix(ctx context.Context, runID string, prefixes ...string) ([]run.Record, error)
}

// WithRecordReader reads a run's records by prefix where only some are
// wanted. Pass one over the ledger WithStopLedger was given. Without it every
// such read loads the whole run, which costs nothing in memory and a full
// read of the run's records on Postgres.
func WithRecordReader(rr RecordReader) Option {
	return func(r *Runtime) { r.reader = rr }
}

// Record key prefixes the runtime reads selectively.
const (
	prefixQuestion = questionStepPrefix
	prefixAnswer   = "event:" + answerRecordPrefix
	prefixStop     = "event:" + stopRecordName
	prefixUsage    = "event:" + usageRecordName
	prefixChild    = childStepPrefix
)

// recordsOf reads the run and those of its records whose key starts with one
// of prefixes: through the RecordReader when there is one, else by loading
// the run whole and keeping what was asked for.
func (r *Runtime) recordsOf(ctx context.Context, runID string, withRun bool, prefixes ...string) (run.Run, []run.Record, error) {
	if r.reader == nil {
		rec, all, err := r.stops.Load(ctx, runID)
		if err != nil {
			return run.Run{}, nil, err
		}
		var out []run.Record
		for _, x := range all {
			for _, p := range prefixes {
				if strings.HasPrefix(x.Key, p) {
					out = append(out, x)
					break
				}
			}
		}
		return rec, out, nil
	}
	var rec run.Run
	if withRun {
		var err error
		if rec, err = r.loadRun(ctx, runID); err != nil {
			return run.Run{}, nil, err
		}
	}
	out, err := r.reader.RecordsWithPrefix(ctx, runID, prefixes...)
	return rec, out, err
}

// Errors an answer can meet.
var (
	// ErrNoQuestion reports a question that does not exist, on a run that
	// does not exist or belongs to another tenant. The three are one error
	// on purpose: telling them apart would confirm a run across tenants.
	ErrNoQuestion = errors.New("runtime: no such question")
	// ErrQuestionClosed reports a question that can no longer be answered:
	// the run was stopped while it waited, or it ended.
	ErrQuestionClosed = errors.New("runtime: the question is closed")
	// ErrAnswerUnavailable reports a runtime that cannot record an answer:
	// it has no ledger (WithStopLedger).
	ErrAnswerUnavailable = errors.New("runtime: answers are not available")
)

// awaitAnswer announces a parked turn's question once and returns its
// resolution, or run.ErrParked while there is none.
//
//nolint:gocyclo // one pass over a parked question and each way it resolves
func (r *Runtime) awaitAnswer(ctx context.Context, at *attemptLog, out Outcome) (resolution, error) {
	q := out.Parked.Question
	runID := at.in.runID()
	// A completed model activity can ask for delegation just as a stop is
	// recorded. Fence that work before creating a child or registering a wait.
	if res, stopped, err := r.stoppedSessionWait(ctx, at.in, q); stopped || err != nil {
		return res, err
	}
	var waits []sessionWait
	isListen := q.Kind == event.QuestionDelegation && q.Tool != nil && q.Tool.Name == "session_listen"
	if isListen {
		var err error
		waits, err = r.prepareSessionListen(ctx, at.in, q)
		if err != nil {
			return resolution{}, err
		}
	}
	if q.Kind == event.QuestionDelegation && !isListen {
		// The child run is started once, as a recorded step: a replay reads
		// its id back, and a retried start lands on the same run, whose id
		// is derived from this run and the question.
		child, err := durable.Step(ctx, childStepPrefix+q.ID, func(ctx context.Context) (string, error) {
			return r.startChild(ctx, at.in, q)
		})
		if err != nil {
			if res, stopped, stopErr := r.stoppedSessionWait(ctx, at.in, q); stopped || stopErr != nil {
				return res, stopErr
			}
			return resolution{}, err
		}
		q.ChildRunID = child
	}
	fresh := false
	waitReason := "waiting on a person"
	if q.Kind == event.QuestionDelegation {
		waitReason = "waiting on sessions"
	}
	if _, err := durable.Step(ctx, questionStepPrefix+q.ID, func(ctx context.Context) (event.Question, error) {
		fresh = true
		if err := at.publish(ctx, event.Parked{
			Attempt: at.attempt, Reason: waitReason, Question: &q,
		}); err != nil {
			return event.Question{}, err
		}
		return q, nil
	}); err != nil {
		return resolution{}, err
	}

	raw, err := durable.Event[json.RawMessage](ctx, answerRecordName(q.ID))
	switch {
	case err == nil:
		return decodeResolution(raw)
	case !errors.Is(err, run.ErrParked):
		return resolution{}, err
	}

	// No resolution in this attempt's records. Three things can still
	// decide it now, and each does so by writing the record and adopting
	// whatever the record then holds.
	if r.stops != nil {
		// A stop recorded on the run ends the wait.
		var stop *native.Stop
		if raw, err := durable.Event[json.RawMessage](ctx, stopRecordName); err == nil {
			var body event.StopRequest
			if json.Unmarshal(raw, &body) == nil && body.RunID == runID {
				stop = &native.Stop{Reason: body.Reason, By: body.By}
			}
		} else if s, ok := r.recordedStop(ctx, runID); ok {
			stop = &s
		}
		if stop != nil {
			return r.resolve(ctx, runID, resolution{QuestionID: q.ID, Stopped: stop})
		}
		if q.Tool != nil && q.Tool.Name == "session_call" {
			var args struct {
				Wait *bool `json:"wait"`
			}
			if json.Unmarshal(q.Tool.Arguments, &args) == nil && args.Wait != nil && !*args.Wait {
				child, err := r.loadRun(ctx, q.ChildRunID)
				if err != nil {
					return resolution{}, err
				}
				var ci Input
				if err := json.Unmarshal(child.Input, &ci); err != nil {
					return resolution{}, err
				}
				result := map[string]string{"run": q.ChildRunID}
				if ci.Session != nil {
					result["session"] = ci.Session.ID
				}
				raw, _ := json.Marshal(result)
				res, err := r.resolve(ctx, runID, resolution{QuestionID: q.ID, Answer: &event.Answer{QuestionID: q.ID, Text: string(raw), At: r.now().UTC()}})
				if err != nil {
					return resolution{}, err
				}
				if fresh {
					r.wake(ctx, runID)
					return resolution{}, run.ErrParked
				}
				return res, nil
			}
		}
		// The rest only in an attempt that replayed the turn rather than ran
		// it: an attempt executes at most one turn, because its sequence
		// band, its usage record and its accounting keys are the attempt's.
		// The attempt that asked parks, and the answer's wake-up starts the
		// next one.
		if !fresh {
			// A resolution written after this attempt loaded its records is
			// read rather than waited for.
			if res, ok := r.storedResolution(ctx, runID, q.ID); ok {
				return res, nil
			}
			if isListen {
				if answer, ok := r.sessionListenEnded(ctx, at.in, q, waits); ok {
					return r.resolve(ctx, runID, resolution{QuestionID: q.ID, Answer: answer})
				}
			}
			// A child run that ended without answering (its retries ran
			// out) answers here, from its record.
			if q.ChildRunID != "" {
				if a, ok := r.childEnded(ctx, q); ok {
					return r.resolve(ctx, runID, resolution{QuestionID: q.ID, Answer: a})
				}
			}
			// A question past its deadline takes its default. Never in the
			// attempt that asked it: a person sees every question first.
			if !q.ExpiresAt.IsZero() && !r.now().Before(q.ExpiresAt) {
				return r.resolve(ctx, runID, resolution{QuestionID: q.ID, Answer: expired(q, r.now())})
			}
		}
	}
	return resolution{}, fmt.Errorf("runtime: run %s is %s for question %s: %w", runID, waitReason, q.ID, err)
}

// expired is a question's default, as the answer it resolves to.
func expired(q event.Question, now time.Time) *event.Answer {
	a := event.Answer{QuestionID: q.ID, Expired: true, At: now.UTC()}
	if q.Default != nil {
		a = *q.Default
		a.QuestionID, a.Expired, a.At = q.ID, true, now.UTC()
	}
	if (q.Kind == event.QuestionApproval || q.Kind == event.QuestionHold) && a.Decision == "" {
		a.Decision = event.DecisionDeny
	}
	return &a
}

// resolve writes a question's resolution, once, and returns what the record
// holds: this one, or whichever was written first.
func (r *Runtime) resolve(ctx context.Context, runID string, res resolution) (resolution, error) {
	body, err := json.Marshal(res)
	if err != nil {
		return resolution{}, fmt.Errorf("runtime: resolve question %s: %w", res.QuestionID, err)
	}
	stored, err := r.stops.Record(context.WithoutCancel(ctx), run.Record{
		RunID: runID, Key: answerRecordKey(res.QuestionID), Kind: run.KindEvent,
		Name: answerRecordName(res.QuestionID), Codec: "application/json",
		Status: run.RecordOK, Output: body,
	})
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, run.ErrAlreadyRecorded):
		return decodeResolution(stored.Output)
	default:
		return resolution{}, fmt.Errorf("runtime: resolve question %s: %w", res.QuestionID, err)
	}
}

// storedResolution reads a question's resolution from the ledger.
func (r *Runtime) storedResolution(ctx context.Context, runID, questionID string) (resolution, bool) {
	_, records, err := r.recordsOf(ctx, runID, false, prefixAnswer)
	if err != nil {
		return resolution{}, false
	}
	for _, rec := range records {
		if rec.Key == answerRecordKey(questionID) && rec.Status == run.RecordOK {
			res, err := decodeResolution(rec.Output)
			return res, err == nil
		}
	}
	return resolution{}, false
}

func decodeResolution(raw []byte) (resolution, error) {
	var res resolution
	if err := json.Unmarshal(raw, &res); err != nil {
		return resolution{}, durable.NonRetryable(fmt.Errorf("runtime: decode a question's resolution: %w", err))
	}
	if res.Answer == nil && res.Stopped == nil {
		return resolution{}, durable.NonRetryable(errors.New("runtime: a question's resolution holds neither an answer nor a stop"))
	}
	return res, nil
}

// stoppedWhileParked ends a run that was stopped while it waited on a person.
// Its turn is not resumed: the run ends with what it had said before it asked,
// and what it had spent.
func (r *Runtime) stoppedWhileParked(ctx context.Context, at *attemptLog, out Outcome, stop native.Stop, parks int) (Output, error) {
	if err := r.closeRunBudget(ctx, at.in); err != nil {
		return Output{}, err
	}
	r.claimStop(ctx, at.in)
	stop.Reason = clip(stop.Reason)
	if stop.Reason == "" {
		stop.Reason = native.DefaultStopReason
	}
	final := Outcome{Text: out.Text, Prompt: out.Prompt, Stopped: &stop, Usage: out.Usage}
	if err := r.remember(ctx, at.in, final); err != nil {
		at.report(ctx, event.Failed{Attempt: at.attempt, Error: err.Error()})
		return Output{}, err
	}
	if err := at.publish(ctx, event.Stopped{
		Attempt: at.attempt, Reason: stop.Reason, By: stop.By, Text: out.Text, Usage: out.Usage,
	}); err != nil {
		return Output{}, err
	}
	r.answerParent(ctx, at.in, out.Text, &stop, nil)
	return Output{
		Text: out.Text, LastSeq: at.band.close, Attempt: at.attempt,
		Events: out.Chunks + 3 + 2*parks + 1, Stopped: &stop, Usage: out.Usage,
	}, nil
}

// AnswerRequest answers one question a run is parked on.
type AnswerRequest struct {
	// Tenant is the caller's, from its credential. A question of another
	// tenant's run is reported as ErrNoQuestion.
	Tenant string
	// RunID is the run.
	RunID string
	// QuestionID is the question, as event.Question.ID reported it.
	QuestionID string
	// Answer is the answer: Decision and optionally Arguments for an
	// approval, Choice for a choice, Text for a reply, and an optional Note.
	// Its QuestionID, By, At and Expired are set here.
	Answer event.Answer
	// By is who answered, from the caller's credential: its subject, or the
	// person a credential trusted to act for others named.
	By string
	// Via is the credential's own subject, whoever it named. A credential
	// never approves a call in a run it raised itself, whoever it names: see
	// "Who may answer" in the documentation.
	Via string
}

// Roles resolves the roles a principal of a tenant holds, for approvers and
// answerers named as "role:<name>". The host owns roles; the runtime only
// asks.
type Roles func(ctx context.Context, tenant, principal string) ([]string, error)

// WithRoles lets a spec's approvers and answerers name roles. Without it an
// entry "role:<name>" admits nobody.
func WithRoles(fn Roles) Option {
	return func(r *Runtime) { r.roles = fn }
}

// ErrNotAllowed reports an answer from someone the question may not be
// answered by: the requester approving their own call, someone outside the
// spec's approvers or answerers, or anyone but the person a call runs as
// approving it with edited arguments.
var ErrNotAllowed = errors.New("runtime: not allowed to answer this question")

// Answer answers a question a run is parked on, and wakes the run.
//
// It is idempotent: the first answer recorded is the answer, and Answer
// returns it to every caller, with first reporting whether it was this call's.
// An answer that arrives after the question expired records the question's
// default instead, and returns that. An answer is checked against the
// question before it is recorded: a decision must be approve or deny, edited
// arguments must satisfy the tool's schema, a choice must be one of the
// options offered, and a reply must not be empty. A call that runs with the
// credential of the person the run acts for (QuestionTool.ActsForPerson) is
// approved with edited arguments only by that person.
//
// It returns ErrNoQuestion for a question this tenant has no run with,
// ErrQuestionClosed when the run was stopped while it waited or has ended
// without the question being answered, an error matching ErrInvalid for an
// answer that does not fit the question, and ErrAnswerUnavailable without
// WithStopLedger.
func (r *Runtime) Answer(ctx context.Context, req AnswerRequest) (event.Answer, bool, error) {
	if r.stops == nil {
		return event.Answer{}, false, fmt.Errorf("%w: no ledger to record it in", ErrAnswerUnavailable)
	}
	req.RunID, req.QuestionID = strings.TrimSpace(req.RunID), strings.TrimSpace(req.QuestionID)
	if req.RunID == "" || req.QuestionID == "" {
		return event.Answer{}, false, fmt.Errorf("%w: an answer names a run and a question", ErrInvalid)
	}
	rec, records, err := r.recordsOf(ctx, req.RunID, true, prefixQuestion, prefixAnswer)
	switch {
	case errors.Is(err, run.ErrNotFound):
		return event.Answer{}, false, ErrNoQuestion
	case err != nil:
		return event.Answer{}, false, fmt.Errorf("runtime: answer %s: read the run: %w", req.RunID, err)
	}
	var in Input
	if err := json.Unmarshal(rec.Input, &in); err != nil || in.Trigger.Tenant != req.Tenant {
		return event.Answer{}, false, ErrNoQuestion
	}
	q, ok := findQuestion(records, req.QuestionID)
	if !ok {
		return event.Answer{}, false, ErrNoQuestion
	}
	for _, rr := range records {
		if rr.Key == answerRecordKey(q.ID) && rr.Status == run.RecordOK {
			return answered(rr.Output, false)
		}
	}
	if rec.Status.Terminal() {
		return event.Answer{}, false, ErrQuestionClosed
	}
	if err := r.mayAnswer(ctx, in, q, req); err != nil {
		return event.Answer{}, false, err
	}

	now := r.now().UTC()
	res := resolution{QuestionID: q.ID}
	if !q.ExpiresAt.IsZero() && !now.Before(q.ExpiresAt) {
		res.Answer = expired(q, now)
	} else {
		a, err := checkAnswer(q, req.Answer)
		if err != nil {
			return event.Answer{}, false, err
		}
		if err := mayEdit(in, q, req, a); err != nil {
			return event.Answer{}, false, err
		}
		a.QuestionID, a.By, a.At, a.Expired = q.ID, clip(req.By), now, false
		res.Answer = &a
	}
	stored, err := r.resolve(ctx, req.RunID, res)
	if err != nil {
		return event.Answer{}, false, err
	}
	// The run may have ended between the check above and the write: failed
	// for good, say. Nothing will ever read the answer then, and saying it
	// was taken would be a lie.
	if after, err := r.loadRun(ctx, req.RunID); err == nil && after.Status == run.StatusFailed {
		return event.Answer{}, false, ErrQuestionClosed
	}
	r.wake(ctx, req.RunID)
	raw, _ := json.Marshal(stored)
	mine, _ := json.Marshal(res)
	return answered(raw, bytes.Equal(raw, mine) && !res.Answer.Expired)
}

// answered returns the answer a resolution record holds.
func answered(raw []byte, first bool) (event.Answer, bool, error) {
	res, err := decodeResolution(raw)
	if err != nil {
		return event.Answer{}, false, err
	}
	if res.Answer == nil {
		return event.Answer{}, false, ErrQuestionClosed
	}
	return *res.Answer, first, nil
}

// findQuestion returns a question a run recorded.
func findQuestion(records []run.Record, id string) (event.Question, bool) {
	for _, rec := range records {
		if rec.Key != questionStepKey(id) || rec.Status != run.RecordOK {
			continue
		}
		var q event.Question
		if json.Unmarshal(rec.Output, &q) == nil && q.ID == id {
			return q, true
		}
	}
	return event.Question{}, false
}

// CheckAnswer reports whether an answer fits its question, as Answer checks
// it: a decision is approve or deny and only an approval carries arguments,
// which must satisfy the tool's schema; a choice is one of the options; a
// reply has text. A host can check an answer before it submits one. Who may
// answer, and who may edit a call's arguments, Answer decides.
func CheckAnswer(q event.Question, a event.Answer) error {
	_, err := checkAnswer(q, a)
	return err
}

// checkAnswer checks an answer against its question and returns the part of
// it that is recorded.
func checkAnswer(q event.Question, a event.Answer) (event.Answer, error) {
	out := event.Answer{Note: clipQuestion(a.Note)}
	switch q.Kind {
	case event.QuestionApproval:
		switch a.Decision {
		case event.DecisionApprove:
		case event.DecisionDeny:
			if len(bytes.TrimSpace(a.Arguments)) > 0 {
				return out, fmt.Errorf("%w: a denial cannot carry arguments", ErrInvalid)
			}
		default:
			return out, fmt.Errorf("%w: an approval is answered %q or %q, not %q", ErrInvalid, event.DecisionApprove, event.DecisionDeny, a.Decision)
		}
		out.Decision = a.Decision
		if args := bytes.TrimSpace(a.Arguments); len(args) > 0 {
			var value map[string]any
			dec := json.NewDecoder(bytes.NewReader(args))
			dec.UseNumber()
			if err := dec.Decode(&value); err != nil || value == nil || dec.More() {
				return out, fmt.Errorf("%w: edited arguments must be a JSON object", ErrInvalid)
			}
			if q.Tool == nil || len(q.Tool.Schema) == 0 {
				return out, errors.New("runtime: the question records no schema to check edited arguments against; they are refused")
			}
			var schema saigetypes.ParameterSchema
			if err := json.Unmarshal(q.Tool.Schema, &schema); err != nil {
				return out, fmt.Errorf("runtime: the question's schema cannot be read, so edited arguments are refused: %w", err)
			}
			if err := saigetypes.ValidateJSON(schema, value); err != nil {
				return out, fmt.Errorf("%w: edited arguments do not fit %s: %w", ErrInvalid, q.Tool.Name, err)
			}
			compact := &bytes.Buffer{}
			if err := json.Compact(compact, args); err != nil {
				return out, fmt.Errorf("%w: edited arguments: %w", ErrInvalid, err)
			}
			out.Arguments = compact.Bytes()
		}
	case event.QuestionChoice:
		if !slices.Contains(q.Options, a.Choice) {
			return out, fmt.Errorf("%w: %q is not one of the options offered", ErrInvalid, a.Choice)
		}
		out.Choice = a.Choice
	case event.QuestionReply:
		text := clipQuestion(a.Text)
		if text == "" {
			return out, fmt.Errorf("%w: a reply needs text", ErrInvalid)
		}
		out.Text = text
	case event.QuestionHold:
		switch a.Decision {
		case event.DecisionApprove, event.DecisionDeny:
		default:
			return out, fmt.Errorf("%w: a hold is answered %q or %q, not %q", ErrInvalid, event.DecisionApprove, event.DecisionDeny, a.Decision)
		}
		if len(bytes.TrimSpace(a.Arguments)) > 0 {
			return out, fmt.Errorf("%w: an answer to a hold carries no arguments", ErrInvalid)
		}
		out.Decision = a.Decision
	case event.QuestionDelegation:
		return out, fmt.Errorf("%w: question %s waits on a child run, and is answered by that run ending", ErrInvalid, q.ID)
	default:
		return out, fmt.Errorf("%w: question %s has kind %q, which this runtime cannot answer", ErrInvalid, q.ID, q.Kind)
	}
	return out, nil
}

// clipQuestion trims an answer's free text and bounds it, on a rune boundary.
func clipQuestion(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= native.MaxQuestionText {
		return s
	}
	end := native.MaxQuestionText
	for end > 0 && s[end]&0xC0 == 0x80 {
		end--
	}
	return s[:end]
}

// wake enqueues a run whose question was resolved. A worker may be settling
// the attempt that parked it at this moment, and an enqueue of a run a worker
// still holds is dropped, so it is repeated twice more shortly after. A run
// already woken ignores the repeats.
func (r *Runtime) wake(ctx context.Context, runID string) {
	if r.waker == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := r.waker.Enqueue(ctx, runID, 0); err != nil {
		r.logger.Warn("flow/runtime: could not wake a parked run; the janitor will",
			"run_id", runID, "error", err)
	}
	go func() {
		for _, d := range []time.Duration{200 * time.Millisecond, time.Second} {
			time.Sleep(d)
			_ = r.waker.Enqueue(ctx, runID, 0)
		}
	}()
}

// PendingQuestion returns the question a run of tenant's is parked on, for a
// host that shows it. A run that is not parked, does not exist or is another
// tenant's reports false. It returns ErrAnswerUnavailable without
// WithStopLedger.
func (r *Runtime) PendingQuestion(ctx context.Context, tenant, runID string) (event.Question, bool, error) {
	if r.stops == nil {
		return event.Question{}, false, fmt.Errorf("%w: no ledger", ErrAnswerUnavailable)
	}
	rec, records, err := r.recordsOf(ctx, runID, true, prefixQuestion, prefixAnswer)
	switch {
	case errors.Is(err, run.ErrNotFound):
		return event.Question{}, false, nil
	case err != nil:
		return event.Question{}, false, fmt.Errorf("runtime: questions of %s: %w", runID, err)
	}
	var in Input
	if err := json.Unmarshal(rec.Input, &in); err != nil || in.Trigger.Tenant != tenant || rec.Status.Terminal() {
		return event.Question{}, false, nil
	}
	q, ok := pendingQuestion(records)
	return q, ok, nil
}

// pendingQuestion is the latest question among a run's records that has no
// resolution.
func pendingQuestion(records []run.Record) (event.Question, bool) {
	resolved := map[string]bool{}
	for _, rec := range records {
		if rec.Kind == run.KindEvent && strings.HasPrefix(rec.Name, answerRecordPrefix) {
			resolved[strings.TrimPrefix(rec.Name, answerRecordPrefix)] = true
		}
	}
	var out event.Question
	found := false
	for _, rec := range records {
		if !isQuestionRecord(rec) || rec.Status != run.RecordOK {
			continue
		}
		var q event.Question
		if json.Unmarshal(rec.Output, &q) != nil || q.ID == "" || resolved[q.ID] {
			continue
		}
		if !found || q.At.After(out.At) {
			out, found = q, true
		}
	}
	return out, found
}

// stopParked resolves the question a run is parked on as stopped and wakes
// the run, which then ends without resuming its turn. It is called by Stop
// once the request is recorded.
func (r *Runtime) stopParked(ctx context.Context, tenant, runID string, body []byte) error {
	_, records, err := r.recordsOf(ctx, runID, false, prefixQuestion, prefixAnswer, prefixChild)
	if err != nil {
		return fmt.Errorf("runtime: stop %s: read its questions: %w", runID, err)
	}
	q, ok := pendingQuestion(records)
	if !ok {
		return nil
	}
	var req event.StopRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("runtime: stop %s: %w", runID, err)
	}
	if _, err := r.resolve(ctx, runID, resolution{QuestionID: q.ID, Stopped: &native.Stop{Reason: req.Reason, By: req.By}}); err != nil {
		return err
	}
	r.wake(ctx, runID)
	// A run waiting on a child it delegated to stops the child too, with the
	// same words: nobody waits for its answer any more.
	for _, rec := range records {
		if rec.Key != childStepPrefix+q.ID+"#0" || rec.Status != run.RecordOK {
			continue
		}
		var child string
		if json.Unmarshal(rec.Output, &child) == nil && child != "" {
			if err := r.Stop(ctx, StopRequest{Tenant: tenant, RunID: child, Reason: req.Reason, By: req.By}); err != nil {
				r.logger.Warn("flow/runtime: could not stop the child run of a stopped run", "run_id", runID, "child", child, "error", err)
			}
		}
	}
	return nil
}

// childQuestionPoll is how often a Delegator's watch reads a child run for a
// question: the run's row and its question and answer records, never its
// transcripts.
const childQuestionPoll = time.Second

type childQuestionKey struct{}

// childQuestion returns how a child run a Delegator started is checked and
// read for a pending question: a run of the parent's tenant, other than the
// parent, in the ledger.
func (r *Runtime) childQuestion(tenant, parentRunID string) func(context.Context, string) (event.ChildParked, bool, bool, error) {
	return func(ctx context.Context, runID string) (event.ChildParked, bool, bool, error) {
		runID = strings.TrimSpace(runID)
		if r.stops == nil {
			return event.ChildParked{}, false, false, fmt.Errorf("%w: watching a child run needs the run ledger (WithStopLedger)", ErrNotAChild)
		}
		if runID == "" || runID == parentRunID {
			return event.ChildParked{}, false, false, fmt.Errorf("%w: %q", ErrNotAChild, runID)
		}
		rec, records, err := r.recordsOf(ctx, runID, true, prefixQuestion, prefixAnswer)
		switch {
		case errors.Is(err, run.ErrNotFound):
			return event.ChildParked{}, false, false, fmt.Errorf("%w: no run %q", ErrNotAChild, runID)
		case err != nil:
			return event.ChildParked{}, false, false, fmt.Errorf("runtime: read child run %s: %w", runID, err)
		}
		var in Input
		if err := json.Unmarshal(rec.Input, &in); err != nil || in.Trigger.Tenant != tenant {
			return event.ChildParked{}, false, false, fmt.Errorf("%w: no run %q", ErrNotAChild, runID)
		}
		if rec.Status.Terminal() {
			return event.ChildParked{}, false, true, nil
		}
		q, ok := pendingQuestion(records)
		return event.ChildParked{RunID: runID, Agent: in.Subscriber, Question: q}, ok, false, nil
	}
}

// WatchChildQuestions shows a child run's questions on the stream of the run
// that delegated to it, for as long as the delegation lasts. A Delegator that
// starts a run of its own for a delegation and waits on it calls it with the
// context it was given:
//
//	acc, err := rt.Accept(ctx, trigger, target)
//	...
//	err = runtime.WatchChildQuestions(ctx, acc.RunID)
//	out, err := rt.Result(ctx, acc.RunID)
//
// Each question the child parks on is published once on the parent's stream
// as run.child.parked, with the child's run id, so a host showing the
// parent shows it waiting on that question, and answers it by answering the
// child run. The watch reads the ledger, and ends when the child ends or the
// delegation returns.
//
// Only the run id comes from the host: the runtime refuses it with
// ErrNotAChild unless it is a run of the parent's tenant other than the
// parent. It needs the run ledger (WithStopLedger) and the Sandbox executor,
// and returns ErrInvalid outside a Delegator's context.
func WatchChildQuestions(ctx context.Context, runID string) error {
	watch, ok := ctx.Value(childQuestionKey{}).(func(context.Context, string) error)
	if !ok || watch == nil {
		return fmt.Errorf("%w: WatchChildQuestions is called with the context a Delegator was given", ErrInvalid)
	}
	return watch(ctx, runID)
}

// watchChild polls a child run for questions and reports each new one, until
// the child ends or ctx does.
func watchChild(ctx context.Context, read func(context.Context, string) (event.ChildParked, bool, bool, error), runID string, report func(event.ChildParked) error) error {
	if _, _, _, err := read(ctx, runID); err != nil {
		return err
	}
	go func() {
		seen := map[string]bool{}
		tick := time.NewTicker(childQuestionPoll)
		defer tick.Stop()
		for {
			cq, ok, ended, err := read(ctx, runID)
			if ended {
				return
			}
			if err == nil && ok && !seen[cq.Question.ID] {
				seen[cq.Question.ID] = true
				if report(cq) != nil {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return nil
}

// mayAnswer decides whether req may answer q, from the run's pinned policy:
// the definition the run was accepted under, never a newer one.
//
// An approval is a second pair of eyes. The person whose request raised the
// run may not approve its calls unless the spec allows self-approval, and
// neither may the credential that raised it, whoever that credential names:
// a surface trusted to act for others (ActAs) vouches for the person it
// names, and that person is checked against the requester like anyone else,
// but a credential that raised a run as itself could otherwise name a
// stranger to approve its own call. Approvers and answerers, when the spec
// names them, are the only people admitted; asked_of on a question is a
// routing hint and admits nobody. An approval question that names approvers
// of its own (set per call by the tool that asked, native.Approving) is
// governed by them and by its own self-approval instead of the spec's.
func (r *Runtime) mayAnswer(ctx context.Context, in Input, q event.Question, req AnswerRequest) error {
	loaded, err := r.defs.Verify(ctx, in.Trigger.Tenant, in.Pin)
	if err != nil {
		return fmt.Errorf("runtime: answer: read agent %q version %s: %w", in.Subscriber, in.Pin.Version, err)
	}
	policy := loaded.Agent.Policy
	by := clip(req.By)
	var admitted []string
	switch q.Kind {
	case event.QuestionApproval:
		self := policy.SelfApproval
		admitted = policy.Approvers
		if len(q.Approvers) > 0 {
			self, admitted = q.SelfApproval, q.Approvers
		}
		if !self {
			requester := requesterOf(in)
			if requester[by] || requester[strings.TrimSpace(req.Via)] {
				return fmt.Errorf("%w: the request that raised this call cannot approve it; another person must", ErrNotAllowed)
			}
		}
	case event.QuestionChoice, event.QuestionReply:
		admitted = policy.Answerers
	case event.QuestionHold:
		// The host's question names its own answerers, or leaves who may
		// answer to the host, which checks before it asks.
		admitted = q.Approvers
	default:
		return nil
	}
	if len(admitted) == 0 {
		return nil
	}
	if by == "" {
		return fmt.Errorf("%w: the answer names nobody, and this question is answered only by named people", ErrNotAllowed)
	}
	var roles map[string]bool
	for _, who := range admitted {
		role, isRole := strings.CutPrefix(who, "role:")
		if !isRole {
			if who == by {
				return nil
			}
			continue
		}
		if roles == nil {
			roles = map[string]bool{}
			if r.roles != nil {
				held, err := r.roles(ctx, in.Trigger.Tenant, by)
				if err != nil {
					return fmt.Errorf("runtime: answer: resolve the roles of %q: %w", by, err)
				}
				for _, h := range held {
					roles[h] = true
				}
			}
		}
		if roles[role] {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not among the people this question is answered by", ErrNotAllowed, by)
}

// requesterOf is every name the person whose event raised a run goes by: the
// actor's id, and the id qualified by the trigger it came through, which is
// how a surface trusted to act for others names people ("slack:U1").
// mayEdit refuses edited arguments on an approval of a call that runs with
// the credential of the person the run acts for, unless that person is the
// one answering. Anyone else's edit would act as them on something they did
// not ask for, at whatever their credential reaches; they may still approve
// or deny the call as it was asked.
func mayEdit(in Input, q event.Question, req AnswerRequest, a event.Answer) error {
	if len(a.Arguments) == 0 || q.Tool == nil || !q.Tool.ActsForPerson || isActor(in, clip(req.By)) {
		return nil
	}
	return fmt.Errorf("%w: %s runs with the credential of the person this run acts for, so only they may change its arguments; approve or deny it as asked",
		ErrNotAllowed, q.Tool.Name)
}

// isActor reports whether by is the person a run acts for: the host's
// account for its actor (Actor.User) when the trigger names one, and
// otherwise the actor under the names the requester goes by. A bot is nobody.
func isActor(in Input, by string) bool {
	actor := in.Actor()
	if by == "" || actor.Bot {
		return false
	}
	if user := strings.TrimSpace(actor.User); user != "" {
		return by == user
	}
	return requesterOf(in)[by]
}

func requesterOf(in Input) map[string]bool {
	out := map[string]bool{}
	if id := strings.TrimSpace(in.Actor().ID); id != "" {
		out[id] = true
		if origin := in.Origin(); origin != "" {
			out[origin+":"+id] = true
		}
	}
	return out
}

// trimTranscript stores a continuation's park as what it added to the
// transcript it was given: across repeated parks of one run the conversation
// is then stored once, in pieces, instead of once per park. A park whose
// transcript does not start with the one it was given (it compacted, or it
// records the whole conversation) is stored whole.
func trimTranscript(parked, given *native.Parked) {
	if parked == nil || given == nil || parked.Whole || given.Whole {
		return
	}
	n := len(given.Transcript)
	if len(parked.Transcript) < n {
		return
	}
	for i := range n {
		if !bytes.Equal(parked.Transcript[i], given.Transcript[i]) {
			return
		}
	}
	parked.Transcript = append([]json.RawMessage(nil), parked.Transcript[n:]...)
	parked.Since = n
}

// wholeTranscript rebuilds a park's whole transcript from the previous one's
// and what this park stored. It reads only recorded outcomes, so a replay
// rebuilds the same bytes.
func wholeTranscript(prev []json.RawMessage, parked *native.Parked) ([]json.RawMessage, error) {
	if parked.Since == 0 {
		return parked.Transcript, nil
	}
	if parked.Since > len(prev) {
		return nil, durable.NonRetryable(fmt.Errorf("runtime: a parked turn continues %d messages of a transcript that has %d", parked.Since, len(prev)))
	}
	out := make([]json.RawMessage, 0, parked.Since+len(parked.Transcript))
	out = append(out, prev[:parked.Since]...)
	return append(out, parked.Transcript...), nil
}

// WithChildRuns makes a delegation by a durable run a run of its own that the
// parent parks on, instead of work the parent's turn waits for. The parent's
// turn ends at the delegation's tool call, the runtime starts the child (the
// target agent's pinned version, the parent's tenant, requester and files,
// no thread), and the parent holds no worker until the child ends, when the
// child's answer, or why it has none, is the tool call's result. A host's
// Delegator is not called for such a run. It needs WithStopLedger.
//
// It is what keeps delegation from exhausting workers: a parent that waited in
// its turn would hold its worker for as long as its child took, including
// while the child waited on a person, and with as many waiting parents as
// workers no child could run.
func WithChildRuns() Option {
	return func(r *Runtime) { r.childRuns = true }
}

// startChild accepts the child run a delegation waits on and returns its id.
//
//nolint:gocyclo // one pass over a child call: pin, admit, start and record
func (r *Runtime) startChild(ctx context.Context, parent Input, q event.Question) (string, error) {
	if stop, err := r.sessionStop(ctx, parent); err != nil {
		return "", err
	} else if stop != nil {
		return "", durable.NonRetryable(ErrStopped)
	}
	if q.Tool == nil {
		return "", durable.NonRetryable(fmt.Errorf("%w: delegation %s names no tool call", ErrInvalid, q.ID))
	}
	target, ok := strings.CutPrefix(q.Tool.Name, "tag_")
	if (!ok || target == "") && q.Tool.Name != "session_call" {
		return "", durable.NonRetryable(fmt.Errorf("%w: %q is not a delegation tool", ErrInvalid, q.Tool.Name))
	}
	var args struct {
		Agent string `json:"agent"`
		Text  string `json:"text"`
		Label string `json:"label,omitempty"`
		Fresh bool   `json:"fresh,omitempty"`
		Wait  *bool  `json:"wait,omitempty"`
	}
	if err := json.Unmarshal(q.Tool.Arguments, &args); err != nil {
		return "", durable.NonRetryable(err)
	}
	if q.Tool.Name == "session_call" {
		target = args.Agent
	}
	in, err := childInput(parent, target, args.Text, q.ID)
	if err != nil {
		return "", durable.NonRetryable(err)
	}
	if args.Fresh {
		args.Label = q.ID
	}
	if args.Wait != nil && !*args.Wait {
		in.Parent.QuestionID = ""
	}
	if err := r.childSession(ctx, parent, &in, args.Label); err != nil {
		return "", durable.NonRetryable(err)
	}
	call := SessionCall{ID: in.Trigger.ID, Agent: target, Text: args.Text, Label: args.Label, Fresh: args.Fresh, Session: session.Record{}, QuestionID: in.Parent.QuestionID}
	if in.Session != nil {
		call.Session = *in.Session
	}
	expected := in.runID()
	if r.childStarter != nil {
		if r.childRunID == nil {
			return "", durable.NonRetryable(fmt.Errorf("%w: governed calls require WithChildRunID", session.ErrDenied))
		}
		expected = r.childRunID(parent, call)
		if expected == "" {
			return "", durable.NonRetryable(session.ErrInvalid)
		}
	}
	admission, err := r.reserveChild(ctx, parent, in, expected)
	if err != nil {
		return "", durable.NonRetryable(err)
	}
	finish := func(id string, err error) (string, error) {
		if id != "" && id != expected {
			return id, durable.NonRetryable(fmt.Errorf("%w: child admission returned another run ID", session.ErrInvalid))
		}
		if admission != nil {
			if id != "" {
				if bindErr := admission.BindReservation(context.WithoutCancel(ctx), in.Trigger.Tenant, in.Session.Key.Parent, admissionKey(in), id); bindErr != nil {
					return id, bindErr
				}
			} else if err != nil {
				if releaseErr := admission.Release(context.WithoutCancel(ctx), in.Trigger.Tenant, in.Session.Key.Parent, admissionKey(in)); releaseErr != nil {
					return "", releaseErr
				}
			}
		}
		if id != "" {
			// Stop may have scanned the reservation before admission existed.
			// Once accepted, reconcile it before acknowledging the child call.
			stop, stopErr := r.sessionStop(context.WithoutCancel(ctx), parent)
			if stopErr != nil {
				return id, stopErr
			}
			if stop != nil {
				if stopErr := r.Stop(context.WithoutCancel(ctx), StopRequest{Tenant: parent.Trigger.Tenant, RunID: id, Reason: stop.Reason, By: stop.By}); stopErr != nil {
					return id, stopErr
				}
			}
		}
		return id, err
	}
	if stop, err := r.sessionStop(ctx, parent); err != nil {
		return finish("", err)
	} else if stop != nil {
		return finish("", durable.NonRetryable(ErrStopped))
	}
	if r.childStarter != nil {
		if in.Session == nil {
			return "", durable.NonRetryable(fmt.Errorf("%w: governed child calls need a session store", ErrInvalid))
		}
		id, err := r.childStarter(ctx, parent, call)
		return finish(id, err)
	}
	if err := r.reserveChildBudget(ctx, parent, &in); err != nil {
		return finish("", durable.NonRetryable(err))
	}
	// The child runs the version its parent pinned for it, when it pinned
	// one; a target outside the pin resolves its latest.
	version, _ := parent.Pin.SubagentVersion(definition.NormalizeName(target))
	acc, err := r.start(context.WithoutCancel(ctx), in, version)
	if err != nil {
		if acc.RunID != "" {
			// Durable, and the janitor will drive it. Complete its session index
			// before the step commits, so a transient index failure is retried.
			if r.sessions != nil && in.Session != nil {
				if err := r.sessions.BindRun(context.WithoutCancel(ctx), in.Trigger.Tenant, in.Session.ID, acc.RunID); err != nil {
					return finish(acc.RunID, err)
				}
			}
			return finish(acc.RunID, nil)
		}
		if in.Budget != nil {
			if store, ok := r.sessions.(session.BudgetStore); ok {
				if e := store.SettleBudget(context.WithoutCancel(ctx), in.Trigger.Tenant, parent.runID(), "child:"+in.runID(), session.Budget{}); e != nil {
					return finish("", e)
				}
			}
		}
		return finish("", fmt.Errorf("runtime: start the child run for %s: %w", q.Tool.Name, err))
	}
	return finish(acc.RunID, nil)
}

// childInput is the input of the child run a delegation starts: a
// message.posted.v1 naming the target, with the parent's requester and files.
// Its id is derived from the parent and the wait, so a retried start joins
// the run the first one started, and its source is not an address, so the
// child has no surface and a thread of its own. It publishes on
// delegation://<tenant>/delegation, where no surface renders it.
func childInput(parent Input, target, text, questionID string) (Input, error) {
	if target == "" {
		return Input{}, fmt.Errorf("%w: delegation names no agent", ErrInvalid)
	}
	parentID := parent.runID()
	from := parent.request()
	tenant := parent.Trigger.Tenant
	body := event.MessagePosted{Request: event.Request{
		Text: text,
		// The requester of the parent is the requester of the child: who
		// may approve its calls is decided against them.
		Actor:       from.Actor,
		Mentions:    []string{target},
		Attachments: attachment.Inherit(from.Attachments, nil),
	}}
	trigger := event.Envelope{
		ID:      "child-" + parentID + "-" + questionID,
		Source:  "delegation:" + parentID,
		Type:    body.Type(),
		Channel: "delegation://" + url.PathEscape(busTenant(tenant)) + "/delegation",
		Time:    parent.Trigger.Time,
		Hops:    parent.Trigger.Hops + 1,
		Tenant:  tenant,
		Data:    body,
	}
	return Input{Trigger: trigger, Subscriber: target, Parent: &ParentRef{RunID: parentID, QuestionID: questionID}}, nil
}

// childEnded answers a delegation from its child run's record, once the
// child has ended.
func (r *Runtime) childEnded(ctx context.Context, q event.Question) (*event.Answer, bool) {
	rec, err := r.loadRun(ctx, q.ChildRunID)
	if err != nil || !rec.Status.Terminal() {
		return nil, false
	}
	a := &event.Answer{QuestionID: q.ID, RunID: q.ChildRunID, At: r.now().UTC()}
	if rec.Status == run.StatusFailed {
		a.Note = "the delegated run failed: " + clip(rec.Error)
		return a, true
	}
	var out Output
	if err := json.Unmarshal(rec.Output, &out); err == nil {
		a.Text = out.Text
		if out.Stopped != nil {
			a.Note = "the delegated run was stopped: " + out.Stopped.Reason
		}
	}
	return a, true
}

// answerParent tells the run that started this one for a delegation how it
// ended, and wakes it. It is best effort: a parent that is not told reads the
// child's ending from the ledger when its janitor poll comes.
func (r *Runtime) answerParent(ctx context.Context, in Input, text string, stopped *native.Stop, failure error) {
	r.wakeSessionListeners(ctx, in)
	if in.Parent == nil || in.Parent.QuestionID == "" || r.stops == nil {
		return
	}
	if _, records, err := r.recordsOf(ctx, in.Parent.RunID, false, prefixQuestion); err == nil {
		for _, record := range records {
			var q event.Question
			if json.Unmarshal(record.Output, &q) == nil && q.ID == in.Parent.QuestionID && q.Tool != nil && q.Tool.Name == "session_call" {
				var args struct {
					Wait *bool `json:"wait"`
				}
				if json.Unmarshal(q.Tool.Arguments, &args) == nil && args.Wait != nil && !*args.Wait {
					r.wake(ctx, in.Parent.RunID)
					return
				}
			}
		}
	}
	a := event.Answer{QuestionID: in.Parent.QuestionID, RunID: in.runID(), Text: text, At: r.now().UTC()}
	switch {
	case failure != nil:
		a.Text, a.Note = "", "the delegated run failed: "+clip(failure.Error())
	case stopped != nil:
		a.Note = "the delegated run was stopped: " + stopped.Reason
	}
	if _, err := r.resolve(ctx, in.Parent.RunID, resolution{QuestionID: in.Parent.QuestionID, Answer: &a}); err != nil {
		r.logger.Warn("flow/runtime: could not answer the parent run; its poll will", "run_id", a.RunID, "parent", in.Parent.RunID, "error", err)
		return
	}
	r.wake(ctx, in.Parent.RunID)
}
