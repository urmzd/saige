package runtime

import (
	"context"
	"encoding/json"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// sessionStop fails closed on a ledger read failure at the child admission
// boundary. An absent stop is meaningful only after that read succeeded.
func (r *Runtime) sessionStop(ctx context.Context, in Input) (*native.Stop, error) {
	if r.stops == nil {
		return nil, nil
	}
	stop, found, err := r.StopRequested(ctx, in.Trigger.Tenant, in.runID())
	if err != nil || !found {
		return nil, err
	}
	return &native.Stop{Reason: stop.Reason, By: stop.By}, nil
}

func (r *Runtime) stoppedSessionWait(ctx context.Context, in Input, q event.Question) (resolution, bool, error) {
	stop, err := r.sessionStop(ctx, in)
	if err != nil || stop == nil {
		return resolution{}, false, err
	}
	body, _ := json.Marshal(event.StopRequest{RunID: in.runID(), Reason: stop.Reason, By: stop.By})
	if err := r.stopSessionChildren(context.WithoutCancel(ctx), in, body); err != nil {
		return resolution{}, false, err
	}
	res, err := r.resolve(ctx, in.runID(), resolution{QuestionID: q.ID, Stopped: stop})
	return res, true, err
}
