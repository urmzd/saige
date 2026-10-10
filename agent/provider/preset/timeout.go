package preset

import (
	"context"
	"time"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// attemptTimeout bounds one provider attempt, from the call until its stream
// closes. It sits inside the retry decorator, so each retry gets a fresh
// deadline. It embeds wrapper.Base, so every optional interface the agent
// loop and the router rely on reaches the inner provider.
type attemptTimeout struct {
	wrapper.Base
	timeout time.Duration
}

func withAttemptTimeout(p types.Provider, d time.Duration) types.Provider {
	if d <= 0 {
		return p
	}
	t := &attemptTimeout{timeout: d}
	t.Base = wrapper.NewBase(p, t.rewrap)
	return t
}

func (t *attemptTimeout) rewrap(inner types.Provider) types.Provider {
	return withAttemptTimeout(inner, t.timeout)
}

func (t *attemptTimeout) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if err := t.Check(req); err != nil {
		return nil, err
	}
	return t.bound(ctx, func(ctx context.Context) (<-chan types.Delta, error) {
		return t.Inner.Stream(ctx, req)
	})
}

// bound runs call under the attempt deadline and keeps the deadline until
// the stream closes. An expired deadline is reported as a transient error so
// the retry and router layers treat it like any other slow attempt.
func (t *attemptTimeout) bound(ctx context.Context, call func(context.Context) (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	actx, cancel := context.WithTimeout(ctx, t.timeout)
	src, err := call(actx)
	if err != nil {
		cancel()
		return nil, t.classify(ctx, actx, err)
	}
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		defer cancel()
		for d := range src {
			if e, ok := d.(types.ErrorDelta); ok {
				d = types.ErrorDelta{Error: t.classify(ctx, actx, e.Error)}
			}
			select {
			case out <- d:
			case <-ctx.Done():
				go func() {
					for range src {
					}
				}()
				return
			}
		}
	}()
	return out, nil
}

// classify turns an error caused by the attempt deadline, not the caller's
// context, into a transient provider error.
func (t *attemptTimeout) classify(parent, actx context.Context, err error) error {
	if err == nil || parent.Err() != nil || actx.Err() != context.DeadlineExceeded {
		return err
	}
	return &types.ProviderError{Provider: t.Name(), Model: t.Model(), Kind: types.ErrorKindTransient,
		Err: errAttemptTimeout{timeout: t.timeout, err: err}}
}

type errAttemptTimeout struct {
	timeout time.Duration
	err     error
}

func (e errAttemptTimeout) Error() string {
	return "attempt exceeded " + e.timeout.String() + ": " + e.err.Error()
}

func (e errAttemptTimeout) Unwrap() error { return e.err }
