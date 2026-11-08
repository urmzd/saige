package runtime

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/event"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
	"github.com/urmzd/saige/flow/turn/sandbox"
)

type requestKey struct{}

type outcomeKey struct{}

// outcomeSlot is where a Sandbox reports the whole outcome of a turn the
// runtime handed it: the stored answer and the parked state turn.Result has
// no field for.
type outcomeSlot struct {
	out Outcome
	set bool
}

// RequestFrom returns the request the runtime runs the turn under ctx for.
// A turn.Runtime that needs more than its pinned definition and input (the
// run id, the pinned policy, the surface) reads it here. ok is false outside
// a turn the runtime started.
func RequestFrom(ctx context.Context) (Request, bool) {
	req, ok := ctx.Value(requestKey{}).(Request)
	return req, ok
}

// WithRequest returns ctx carrying req as the request of the turn under it.
// A turn.Runtime that decorates another uses it to hand on a request it has
// changed, such as a definition with the host's model policy applied.
func WithRequest(ctx context.Context, req Request) context.Context {
	return context.WithValue(ctx, requestKey{}, req)
}

// report hands the whole outcome of a turn back to the runtime that started
// it. It does nothing for a turn the runtime did not start.
func report(ctx context.Context, out Outcome) {
	if slot, ok := ctx.Value(outcomeKey{}).(*outcomeSlot); ok {
		slot.out, slot.set = out, true
	}
}

// runOne runs one turn of req on the runtime's turn.Runtime and returns its
// outcome. The stop and compact signals and the actor reach it on ctx, where
// the runtime put them.
func (r *Runtime) runOne(ctx context.Context, in Input, req Request, emit func(context.Context, event.Body) error) (Outcome, error) {
	tin := turnrt.Input{
		Trigger:      triggerOf(in, req),
		Budget:       req.Budget,
		History:      req.History,
		Instructions: req.Instructions,
		Tenant:       req.Tenant,
		Accounting:   req.Accounting,
		Park:         req.Park,
		QuestionTTL:  req.QuestionTTL,
		Compaction:   native.CompactionTo(req.Compaction),
	}
	if req.Resume != nil {
		p, err := native.ParkedTo(*req.Resume)
		if err != nil {
			return Outcome{}, err
		}
		tin.Resume = &p
	}
	slot := &outcomeSlot{}
	ctx = context.WithValue(context.WithValue(ctx, requestKey{}, req), outcomeKey{}, slot)
	bodies := 0
	res, err := r.rt.Turn(ctx, req.Definition, tin, func(ctx context.Context, b event.Body) error {
		bodies++
		if emit == nil {
			return nil
		}
		return emit(ctx, b)
	})
	if slot.set {
		return slot.out, err
	}
	out := Outcome{Text: res.Text, Prompt: res.Prompt, Chunks: bodies, Citations: res.Citations, Stopped: res.Stopped}
	if err != nil {
		return out, err
	}
	if res.Parked != nil {
		p, err := native.ParkedOf(*res.Parked)
		if err != nil {
			return out, err
		}
		out.Parked = &p
	}
	return out, nil
}

// triggerOf is the run's trigger as a turn sees it: stamped with the run and
// the pinned definition.
func triggerOf(in Input, req Request) event.Envelope {
	t := in.Trigger
	t.Run = req.RunID
	t.Agent = req.Definition.Name
	t.Version = req.Definition.Version
	t.Digest = req.Definition.Digest
	return t
}

// defaultRuntime is sandbox.Sandboxed(native) over a local sandbox plane
// whose workspace is a fresh temporary directory, made on the first turn.
func defaultRuntime() turnrt.Runtime {
	var (
		once  sync.Once
		inner turnrt.Runtime
		err   error
	)
	return turnrt.Func(func(ctx context.Context, def turnrt.Pinned, in turnrt.Input, emit func(context.Context, event.Body) error) (turnrt.Result, error) {
		once.Do(func() {
			dir, derr := os.MkdirTemp("", "fiber-workspace-")
			if derr != nil {
				err = fmt.Errorf("runtime: default sandbox workspace: %w", derr)
				return
			}
			ws, werr := workspace.NewLocal(dir)
			if werr != nil {
				err = fmt.Errorf("runtime: default sandbox workspace: %w", werr)
				return
			}
			tools := tool.NewRegistry()
			plane := sandbox.Plane{Control: controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop()), Tools: tools}
			inner = sandbox.Sandboxed(&native.Runtime{}, plane)
		})
		if err != nil {
			return turnrt.Result{}, err
		}
		return inner.Turn(ctx, def, in, emit)
	})
}
