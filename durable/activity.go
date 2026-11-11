package durable

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/durable/replay"
	"github.com/urmzd/saige/durable/run"
)

// ActivityFn is a durable function handle. Declare once at package level —
// the Go analogue of a decorator:
//
//	var ChargePayment = durable.Activity("charge-payment", chargePayment)
//
// The name is the correlation contract: records belong to it, releases roll
// forward against it, and a breaking input/output change means a new name
// ("charge-payment.v2") — the one deliberate act.
type ActivityFn[I, O any] struct {
	name string
	fn   func(context.Context, I) (O, error)
}

// Activity wraps fn as a named durable function and registers it in
// run.DefaultRegistry. Duplicate names panic at init.
func Activity[I, O any](name string, fn func(context.Context, I) (O, error)) *ActivityFn[I, O] {
	return ActivityIn(run.DefaultRegistry, name, fn)
}

// ActivityIn registers into a specific registry — test isolation, or fleets
// serving disjoint activity sets.
func ActivityIn[I, O any](reg *run.Registry, name string, fn func(context.Context, I) (O, error)) *ActivityFn[I, O] {
	a := &ActivityFn[I, O]{name: name, fn: fn}
	reg.Register(name, a.erased)
	return a
}

// Name returns the registered activity name.
func (a *ActivityFn[I, O]) Name() string { return a.name }

// Call executes the activity: memoized inside a run, a plain function call
// outside one.
func (a *ActivityFn[I, O]) Call(ctx context.Context, in I) (O, error) {
	return a.call(ctx, "", in)
}

// CallKeyed executes the activity under an explicit record key. Explicit
// keys are order-independent — the escape hatch for calls whose program
// order may vary (keyed fan-out) — and become part of the idempotency key
// handed downstream.
func (a *ActivityFn[I, O]) CallKeyed(ctx context.Context, key string, in I) (O, error) {
	return a.call(ctx, key, in)
}

func (a *ActivityFn[I, O]) call(ctx context.Context, key string, in I) (O, error) {
	var zero O
	f, ok := replay.FromContext(ctx)
	if !ok {
		return a.fn(ctx, in) // pass-through: not under anchor
	}
	codec := f.Codec()
	inB, err := codec.Marshal(in)
	if err != nil {
		return zero, fmt.Errorf("durable: marshal input for %q: %w", a.name, err)
	}
	outB, err := f.Do(ctx, run.KindActivity, a.name, key, inB, func(ctx context.Context) ([]byte, error) {
		out, err := a.fn(ctx, in)
		if err != nil {
			return nil, err
		}
		b, merr := codec.Marshal(out)
		if merr != nil {
			return nil, run.NonRetryable(fmt.Errorf("durable: marshal output for %q: %w", a.name, merr))
		}
		return b, nil
	})
	if err != nil {
		return zero, err
	}
	var out O
	if err := codec.Unmarshal(outB, &out); err != nil {
		// A recorded payload that no longer decodes is deterministic
		// corruption; retrying cannot fix it.
		return zero, run.NonRetryable(&DecodeError{Name: a.name, Err: err})
	}
	return out, nil
}

// erased is the type-erased form registered for workers: bytes in, bytes
// out, frame already in ctx. Generics stop here.
func (a *ActivityFn[I, O]) erased(ctx context.Context, input []byte) ([]byte, error) {
	codec := run.Codec(run.JSONCodec{})
	if f, ok := replay.FromContext(ctx); ok {
		codec = f.Codec()
	}
	var in I
	if err := codec.Unmarshal(input, &in); err != nil {
		return nil, run.NonRetryable(&DecodeError{Name: a.name, Err: err})
	}
	out, err := a.fn(ctx, in)
	if err != nil {
		return nil, err
	}
	b, err := codec.Marshal(out)
	if err != nil {
		return nil, run.NonRetryable(fmt.Errorf("durable: marshal output for %q: %w", a.name, err))
	}
	return b, nil
}
