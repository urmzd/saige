package local

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/agent/durable/anchor"
	"github.com/urmzd/saige/agent/types"
)

// Step is the recorded outcome of one durable step.
type Step struct {
	// Status is completed, running (started on an attempt that has not
	// ended) or indeterminate (may have taken effect; see Reconcile).
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	// Result is a durablecodec record of the step's types.StepResult. For an
	// indeterminate step it holds the budget receipt saved before dispatch.
	Result []byte `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Interrupt is a pending or decided approval or interrupt of a run.
type Interrupt struct {
	Request     types.ApprovalRequest   `json:"request"`
	CreatedAt   time.Time               `json:"created_at"`
	ExpiresAt   time.Time               `json:"expires_at"`
	Decision    *types.ApprovalDecision `json:"decision,omitempty"`
	DecisionKey string                  `json:"decision_key,omitempty"`
	DecidedAt   time.Time               `json:"decided_at,omitzero"`
	// Posted holds an interrupt raised through an InterruptRouter. It is nil
	// for approvals raised through ResolveApproval, which use Request.
	Posted *types.Interrupt `json:"posted,omitempty"`
	// Answer is the free-form reply to a clarification or input interrupt.
	Answer json.RawMessage `json:"answer,omitempty"`
}

// InputSegment is one batch of messages appended to a run's input log after
// its first input. Segments run in order, each after the previous one
// finishes, so a message appended while the run is suspended waits like a
// queued message.
type InputSegment struct {
	Sequence int       `json:"sequence"`      // 1-based; the first input is segment 0
	Key      string    `json:"key,omitempty"` // idempotency key supplied to Append
	At       time.Time `json:"at"`
	Messages []byte    `json:"messages"` // durablecodec record
}

// Event is one entry of a run's history, rendered from its records (see
// anchor.Events).
type Event struct {
	Sequence int       `json:"sequence"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	ID       string    `json:"id,omitempty"`
}

// State is a detached inspection snapshot, derived from the run's records.
// Input and step Result are durablecodec records.
type State struct {
	// History lists what happened, oldest first: steps, approvals and their
	// decisions, appended input, reconciliations, the run's end.
	History  []Event `json:"history"`
	Version  int     `json:"version"`
	RunID    string  `json:"run_id"`
	Revision string  `json:"revision"`
	// Status is ready, running, suspended, completed, failed or cancelled.
	Status     string               `json:"status"`
	Input      []byte               `json:"input"`            // first input segment, a durablecodec record
	Inputs     []InputSegment       `json:"inputs,omitempty"` // later segments, in append order
	Steps      map[string]Step      `json:"steps"`
	Interrupts map[string]Interrupt `json:"interrupts"`
	UpdatedAt  time.Time            `json:"updated_at"`
	Error      string               `json:"error,omitempty"`
}

// stateVersion is the State layout derived from the durable engine's records.
const stateVersion = 2

// state presents an anchor snapshot in this package's form.
func (e *Engine) state(s anchor.State) State {
	out := State{
		Version: stateVersion, RunID: s.RunID, Revision: strings.TrimPrefix(s.Workflow, workflowPrefix),
		Error: s.Error, Steps: map[string]Step{}, Interrupts: map[string]Interrupt{},
		UpdatedAt: s.CompletedAt,
	}
	for i, ev := range s.History {
		out.History = append(out.History, Event{Sequence: i + 1, At: ev.At, Kind: ev.Kind, ID: ev.ID})
		if ev.At.After(out.UpdatedAt) {
			out.UpdatedAt = ev.At
		}
	}
	for _, in := range s.Inputs {
		raw, err := encode(in.Messages)
		if err != nil {
			continue
		}
		if in.Sequence == 0 {
			out.Input = raw
			continue
		}
		out.Inputs = append(out.Inputs, InputSegment{Sequence: in.Sequence, Key: in.Key, At: in.At, Messages: raw})
	}
	for _, st := range s.Steps {
		step := Step{Status: statusRunning, StartedAt: st.StartedAt, CompletedAt: st.CompletedAt, Error: st.Error}
		switch {
		case st.Done:
			step.Status = statusCompleted
		case st.Uncertain:
			step.Status = "indeterminate"
		}
		if st.Result != nil {
			if raw, err := encode(*st.Result); err == nil {
				step.Result = raw
			}
		}
		out.Steps[st.Name] = step
	}
	for _, is := range s.Interrupts {
		in := Interrupt{CreatedAt: is.Interrupt.CreatedAt, ExpiresAt: is.Interrupt.ExpiresAt}
		if is.Request != nil {
			in.Request = *is.Request
		} else {
			posted := is.Interrupt
			in.Posted = &posted
		}
		if is.Reply != nil {
			decision := is.Reply.Decision
			in.Decision, in.DecisionKey, in.DecidedAt, in.Answer = &decision, is.Reply.IdempotencyKey, is.RepliedAt, is.Reply.Answer
		}
		out.Interrupts[is.Interrupt.ID] = in
	}
	switch {
	case s.Cancelled:
		out.Status = statusCancelled
	case s.Status == run.StatusSucceeded, s.Idle:
		out.Status = statusCompleted
	case s.Status == run.StatusFailed:
		out.Status = statusFailed
	case e.running(s.RunID):
		out.Status = statusRunning
	case len(s.Pending) > 0:
		out.Status = statusSuspended
	case len(s.Uncertain) > 0:
		out.Status = statusFailed
		u := s.Uncertain[0]
		out.Error = fmt.Sprintf("%s: %s: %s", ErrIndeterminate, u.Step, u.Error)
	default:
		out.Status = statusReady
	}
	return out
}

// running reports whether a process executes the run now.
func (e *Engine) running(id string) bool {
	leased, err := e.leased(context.Background(), id)
	return err == nil && leased
}
