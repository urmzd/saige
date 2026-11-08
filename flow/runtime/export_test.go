package runtime

import (
	"context"

	"github.com/urmzd/saige/flow/event"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

// Execute runs one turn of req on the sandbox, as the runtime would.
func (s *Sandbox) Execute(ctx context.Context, req Request, emit func(context.Context, event.Body) error) (Outcome, error) {
	return s.execute(ctx, req, emit)
}

// Fake is a test turn written over the runtime's request and outcome.
type Fake interface {
	Execute(ctx context.Context, req Request, emit func(context.Context, event.Body) error) (Outcome, error)
}

// AsRuntime runs a Fake as a turn.Runtime whose whole outcome the runtime
// keeps.
func AsRuntime(f Fake) turnrt.Runtime {
	return turnrt.Func(func(ctx context.Context, _ turnrt.Pinned, _ turnrt.Input, emit func(context.Context, event.Body) error) (turnrt.Result, error) {
		req, _ := RequestFrom(ctx)
		out, err := f.Execute(ctx, req, emit)
		report(ctx, out)
		res := turnrt.Result{Text: out.Text, Prompt: out.Prompt, Citations: out.Citations, Stopped: out.Stopped}
		if out.Parked != nil {
			if p, perr := native.ParkedTo(*out.Parked); perr == nil {
				res.Parked = &p
			}
		}
		return res, err
	})
}
