package preset

import (
	"context"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
	"github.com/urmzd/saige/agent/types"
)

// attemptTimeout bounds one provider attempt, from the call until its stream
// closes. It sits inside the retry decorator, so each retry gets a fresh
// deadline. It forwards the optional interfaces the agent loop and the router
// rely on.
type attemptTimeout struct {
	Inner   types.Provider
	timeout time.Duration
}

var (
	_ types.StructuredOutputProvider = (*attemptTimeout)(nil)
	_ types.OptionsProvider          = (*attemptTimeout)(nil)
	_ types.CapabilityReporter       = (*attemptTimeout)(nil)
	_ types.OptionsReporter          = (*attemptTimeout)(nil)
	_ types.NamedProvider            = (*attemptTimeout)(nil)
	_ types.ModelProvider            = (*attemptTimeout)(nil)
)

func withAttemptTimeout(p types.Provider, d time.Duration) types.Provider {
	if d <= 0 {
		return p
	}
	return &attemptTimeout{Inner: p, timeout: d}
}

// Unwrap returns the inner provider. See package wrapper.
func (t *attemptTimeout) Unwrap() types.Provider { return t.Inner }

// Close closes the inner provider.
func (t *attemptTimeout) Close() error { return types.CloseProvider(t.Inner) }

func (t *attemptTimeout) Name() string  { return types.NameOf(t.Inner) }
func (t *attemptTimeout) Model() string { return types.ProviderModel(t.Inner) }

func (t *attemptTimeout) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(t.Inner)
	return caps
}

func (t *attemptTimeout) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(t.Inner)
	return o
}

func (t *attemptTimeout) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil && !types.AcceptsSchema(t.Inner) {
		return nil, schemacheck.Unsupported(t.Inner, "the inner provider has no structured output")
	}
	if req.Options != nil && !types.AcceptsOptions(t.Inner) {
		return nil, optionscheck.Unsupported(t.Inner)
	}
	return t.bound(ctx, func(ctx context.Context) (<-chan types.Delta, error) {
		return t.Inner.Stream(ctx, req)
	})
}

func (t *attemptTimeout) SupportsSchema() bool { return true }

func (t *attemptTimeout) SupportsOptions() bool { return true }

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
