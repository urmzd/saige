package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/durable"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/event/legacy"
)

// errBeforeUnion is why a run recorded before the event union ends failed.
// It reaches the ledger and run.failed.v1 as text.
var errBeforeUnion = errors.New("started before the event union; send the message again")

// v1Input is a run's input before the event union: the tag, its revision
// and, for a child run, its parent. The tag is read through legacy.DecodeTag.
type v1Input struct {
	Tag    json.RawMessage `json:"tag"`
	Parent *ParentRef      `json:"parent,omitempty"`
}

// failRunFromBeforeUnion is the workflow registered under the turn's names
// from before the event union (fiber.turn) and before the rename
// (oldnames.FiberTurnBeforeRename). It is the only code that runs for such a run, and all it
// does is end it.
//
// That run recorded a tag and a revision number, and the current turn reads
// an event and a pin, so replaying it would run a definition it never pinned.
// Without this registration durable's worker would retry the unknown name and
// then fail the run as "unknown workflow" with nothing published, so a
// surface showing the run would show it in progress forever. Here it fails
// once, terminally, with a reason on the ledger, on its channel and in the
// log. A parked run is reached the same way: the worker's janitor re-enqueues
// every pending run.
func (r *Runtime) failRunFromBeforeUnion(ctx context.Context, old v1Input) (Output, error) {
	attempt := attemptOf(ctx)
	in := Input{Parent: old.Parent}
	if tr, err := legacy.DecodeTag(old.Tag); err == nil {
		in.Trigger, in.Subscriber = tr.Event, tr.Agent
	}
	runID := in.runID()
	err := durable.NonRetryable(fmt.Errorf("runtime: run %s: %w", runID, errBeforeUnion))
	r.logger.Error("flow/runtime: failing a run recorded before the event union",
		"run_id", runID, "agent", in.Subscriber, "tenant", in.Trigger.Tenant)
	if b, berr := newBand(r.stride, attempt); berr == nil {
		at := &attemptLog{r: r, in: in, band: b, attempt: attempt}
		at.report(ctx, event.Failed{Attempt: attempt, Error: err.Error(), Terminal: true})
	}
	// A child run from before the union has a parent from before it, which
	// is failed the same way when this wakes it.
	r.answerParent(ctx, in, "", nil, err)
	return Output{}, err
}

// failActivityFromBeforeUnion is registered under the agent and resume
// activities' names from before the event union. Only a v1 turn calls them,
// and that turn fails before it does; this keeps the names from ever running
// anything.
func (r *Runtime) failActivityFromBeforeUnion(context.Context, json.RawMessage) (Outcome, error) {
	return Outcome{}, durable.NonRetryable(fmt.Errorf("runtime: %w", errBeforeUnion))
}
