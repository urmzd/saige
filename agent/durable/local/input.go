package local

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// InputSegment is one batch of messages appended to a run's input log after
// its first input. Segments run in order, each after the previous one
// finishes, so a message appended while the run is suspended waits like a
// queued message.
type InputSegment struct {
	Sequence int       `json:"sequence"`      // 1-based; the first input is segment 0
	Key      string    `json:"key,omitempty"` // idempotency key supplied to Append
	At       time.Time `json:"at"`
	Messages []byte    `json:"messages"` // gob
}

// segmentPrefix namespaces step names and interrupt IDs of segment i. The
// first segment keeps unprefixed names, so runs recorded before the input
// log existed replay unchanged. Later segments need a prefix because the
// agent restarts its step counter on every call.
func segmentPrefix(i int) string {
	if i == 0 {
		return ""
	}
	return fmt.Sprintf("input-%d/", i)
}

// inputLog decodes every segment of s.
func inputLog(s *State) ([][]types.Message, error) {
	var first []types.Message
	if err := decode(s.Input, &first); err != nil {
		return nil, fmt.Errorf("decode input: %w", err)
	}
	log := [][]types.Message{first}
	for _, seg := range s.Inputs {
		var msgs []types.Message
		if err := decode(seg.Messages, &msgs); err != nil {
			return nil, fmt.Errorf("decode input segment %d: %w", seg.Sequence, err)
		}
		log = append(log, msgs)
	}
	return log, nil
}

// normalize round-trips messages through gob, so values compare the way the
// log stores them (gob drops empty slices and normalizes interface values).
func normalize(msgs []types.Message) ([]byte, []types.Message, error) {
	raw, err := encode(msgs)
	if err != nil {
		return nil, nil, err
	}
	var out []types.Message
	if err := decode(raw, &out); err != nil {
		return nil, nil, err
	}
	return raw, out, nil
}

// reconcileInput checks input against the log in s and appends any messages
// past the end of the log as a new segment. It returns the full log.
func reconcileInput(s *State, input []types.Message) ([][]types.Message, error) {
	log, err := inputLog(s)
	if err != nil {
		return nil, err
	}
	_, current, err := normalize(input)
	if err != nil {
		return nil, err
	}
	i := 0
	for _, seg := range log {
		for _, logged := range seg {
			if i == len(current) {
				return log, nil // input is a prefix of the log
			}
			if !reflect.DeepEqual(logged, current[i]) {
				return nil, fmt.Errorf("%w: input message %d differs from the input log", ErrConflict, i)
			}
			i++
		}
	}
	if i == len(current) {
		return log, nil
	}
	extra := current[i:]
	if err := appendSegment(s, "", extra); err != nil {
		return nil, err
	}
	return append(log, extra), nil
}

// appendSegment adds msgs to the log in s as a new segment.
func appendSegment(s *State, key string, msgs []types.Message) error {
	raw, err := encode(msgs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	seq := len(s.Inputs) + 1
	s.Inputs = append(s.Inputs, InputSegment{Sequence: seq, Key: key, At: now, Messages: raw})
	s.History = append(s.History, Event{Sequence: len(s.History) + 1, At: now, Kind: "input.appended", ID: fmt.Sprint(seq)})
	return nil
}

// Append adds messages to a run's input log without running it. The next Run
// executes them after the logged input finishes. key makes the call
// idempotent: repeating it with the same messages is a no-op, and with
// different messages returns ErrConflict. A cancelled run returns ErrClosed,
// and a run with a live worker returns ErrBusy.
func (e *Engine) Append(id, revision, key string, msgs []types.Message) error {
	if key == "" {
		return errors.New("append idempotency key required")
	}
	if len(msgs) == 0 {
		return errors.New("append requires at least one message")
	}
	_, current, err := normalize(msgs)
	if err != nil {
		return err
	}
	err = e.update(id, revision, func(s *State) error {
		if s.Status == statusCancelled {
			return ErrClosed
		}
		for _, seg := range s.Inputs {
			if seg.Key != key {
				continue
			}
			var logged []types.Message
			if err := decode(seg.Messages, &logged); err != nil {
				return err
			}
			if !reflect.DeepEqual(logged, current) {
				return fmt.Errorf("%w: append key %q reused with different messages", ErrConflict, key)
			}
			return nil
		}
		if err := appendSegment(s, key, current); err != nil {
			return err
		}
		if s.Status == statusCompleted {
			// The run has new work, so it is no longer complete.
			s.Status = statusReady
		}
		return nil
	})
	if err != nil {
		return err
	}
	return signal(context.Background(), e.Notifier, Signal{RunID: id, Kind: SignalInput, ID: key})
}

// runSegments replays or runs each input segment in order on one agent, so
// later segments continue the conversation the earlier ones built.
func runSegments(ctx context.Context, a *agent.Agent, r *runner, log [][]types.Message) (*types.AssistantMessage, error) {
	var result *types.AssistantMessage
	for i, msgs := range log {
		var step types.StepRunner = r
		if prefix := segmentPrefix(i); prefix != "" {
			step = segmentRunner{r: r, prefix: prefix}
		}
		var err error
		result, err = a.RunDurable(ctx, step, msgs, "")
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// segmentRunner runs one later input segment against the shared run state,
// namespacing step names and interrupt IDs under the segment prefix. It
// forwards every optional runner capability the agent looks for.
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
