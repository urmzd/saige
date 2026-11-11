package durable

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/durable/replay"
	"github.com/urmzd/saige/durable/run"
)

// Event is wait-as-absence: it returns the named event record's payload if
// one exists, and parks the run if not. There is no suspend mechanism — an
// event is simply a record that doesn't exist yet. The run stops at its
// frontier (the worker settles the queue item; the run stays pending) and
// resumes when anything writes the record and enqueues the run: a
// Client.Signal call, a row inserted into your own table plus an enqueue, or
// the janitor's backstop poll.
//
// The returned run.ErrParked must propagate: return it up like any error.
//
// Call Event from the workflow body (the function handed to Start), not
// from inside a nested activity: record keys are parent-scoped, and
// Client.Signal targets top-level "event:<name>#<k>" keys. An event awaited
// inside a nested activity needs its prefixed record written by hand.
func Event[O any](ctx context.Context, name string) (O, error) {
	var zero O
	f, ok := replay.FromContext(ctx)
	if !ok {
		return zero, fmt.Errorf("durable: Event %q outside a run has no event source", name)
	}
	outB, err := f.Do(ctx, run.KindEvent, name, "", nil, nil)
	if err != nil {
		return zero, err
	}
	var out O
	if err := f.Codec().Unmarshal(outB, &out); err != nil {
		return zero, run.NonRetryable(&DecodeError{Name: name, Err: err})
	}
	return out, nil
}

// Emit appends a delta to the run's stream, tagged with the activity
// currently executing. Deltas are advisory flow — sealed or superseded by
// checkpoints, streamed to watchers, never truth. Outside a run, or when the
// queue has no DeltaLog capability, Emit is a no-op.
func Emit(ctx context.Context, payload any) error {
	f, ok := replay.FromContext(ctx)
	if !ok {
		return nil
	}
	b, err := f.Codec().Marshal(payload)
	if err != nil {
		return fmt.Errorf("durable: marshal delta: %w", err)
	}
	return f.Emit(ctx, b)
}

// PriorDeltas returns the payloads the currently-executing activity emitted
// on previous attempts — raw material for provider-side resume (for example,
// prompt-prefix continuation of an interrupted stream). The read is exposed;
// nothing is applied automatically.
func PriorDeltas(ctx context.Context) ([][]byte, error) {
	f, ok := replay.FromContext(ctx)
	if !ok {
		return nil, nil
	}
	return f.PriorDeltas(ctx)
}
