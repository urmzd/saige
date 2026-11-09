package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/event"
)

// AcceptOwnChild records the lineage of a child already admitted by the host's
// call policy. Unlike a session wait, it does not answer or wake the parent.
// The parent must be an actual durable run in this tenant; event metadata does
// not establish parentage. The host remains responsible for admission/budget.
func (r *Runtime) AcceptOwnChild(ctx context.Context, trigger event.Envelope, subscriber, parentRun string) (Acceptance, error) {
	if err := trigger.Validate(); err != nil {
		return Acceptance{}, err
	}
	if event.IsControl(trigger.Channel) || event.Family(trigger.Type) == "run" || subscriber == "" || r.stops == nil {
		return Acceptance{}, fmt.Errorf("%w: invalid host child admission", ErrInvalid)
	}
	childID := RunID(trigger.Tenant, trigger.ID)
	if parentRun == "" || parentRun == childID {
		return Acceptance{}, ErrNotAChild
	}
	parent, _, err := r.stops.Load(ctx, parentRun)
	if errors.Is(err, run.ErrNotFound) {
		return Acceptance{}, ErrNotAChild
	}
	if err != nil {
		return Acceptance{}, err
	}
	var held Input
	if json.Unmarshal(parent.Input, &held) != nil || held.Trigger.Tenant != trigger.Tenant {
		return Acceptance{}, ErrNotAChild
	}
	if child, _, err := r.stops.Load(ctx, childID); err == nil {
		if child.ParentRunID != parentRun {
			return Acceptance{}, ErrNotAChild
		}
	} else if !errors.Is(err, run.ErrNotFound) {
		return Acceptance{}, err
	}
	lineage, err := childLineage(LineageFrom(ctx), held, parentRun)
	if err != nil {
		return Acceptance{}, err
	}
	acc, startErr := r.start(ctx, Input{Trigger: trigger, Subscriber: subscriber, Parent: &ParentRef{RunID: parentRun}, Lineage: lineage}, "")
	if acc.RunID == "" {
		return acc, startErr
	}
	// Another replica may have committed the same event while this admission
	// was resolving its definition. The first durable parent remains the owner.
	child, _, err := r.stops.Load(ctx, acc.RunID)
	if err != nil {
		return Acceptance{}, err
	}
	if child.ParentRunID != parentRun {
		return Acceptance{}, ErrNotAChild
	}
	return acc, startErr
}
