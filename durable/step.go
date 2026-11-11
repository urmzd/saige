package durable

import (
	"context"

	"github.com/urmzd/saige/durable/replay"
	"github.com/urmzd/saige/durable/run"
)

// Step records inline non-determinism: time, random values, generated IDs,
// one-off reads. Inside a run it executes once ever — the first recorded
// value is the value forever. Outside a run it just runs fn.
//
//	at, err := durable.Step(ctx, "completed-at",
//	    func(context.Context) (time.Time, error) { return time.Now(), nil })
//
// Give distinct names to steps whose program order may vary: steps carry no
// input hash, so a same-name reorder cannot be detected the way it is for
// activities.
func Step[O any](ctx context.Context, name string, fn func(context.Context) (O, error)) (O, error) {
	var zero O
	f, ok := replay.FromContext(ctx)
	if !ok {
		return fn(ctx)
	}
	codec := f.Codec()
	outB, err := f.Do(ctx, run.KindStep, name, "", nil, func(ctx context.Context) ([]byte, error) {
		v, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		b, merr := codec.Marshal(v)
		if merr != nil {
			return nil, run.NonRetryable(&DecodeError{Name: name, Err: merr})
		}
		return b, nil
	})
	if err != nil {
		return zero, err
	}
	var out O
	if err := codec.Unmarshal(outB, &out); err != nil {
		return zero, run.NonRetryable(&DecodeError{Name: name, Err: err})
	}
	return out, nil
}
