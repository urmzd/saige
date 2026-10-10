package fallback

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
	"github.com/urmzd/saige/agent/types"
)

// Provider tries providers in order, falling back on failure.
// By default it falls back on every error except the terminal ones listed at
// DefaultFallbackOn. Set FallbackOn to control which errors trigger fallback
// (e.g. types.IsTransient for transient-only).
//
// Fallback covers both an immediate Stream error and a mid-stream error
// (an ErrorDelta) that arrives before any content-bearing delta has been
// delivered downstream (see isContentDelta). Once content has been forwarded,
// falling back would duplicate partial output, so the error delta is
// propagated as-is instead.
type Provider struct {
	Config
}

// Config lists a fallback chain's members and its fallback rule.
type Config struct {
	// Providers are the members, tried in order. At least one is required.
	Providers []types.Provider
	// FallbackOn decides whether an error moves on to the next member. Nil
	// means DefaultFallbackOn.
	FallbackOn func(error) bool
}

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithFallbackOn sets Config.FallbackOn.
func WithFallbackOn(fn func(error) bool) Option { return func(c *Config) { c.FallbackOn = fn } }

// DefaultFallbackOn is the policy used when FallbackOn is nil. It falls back
// on every error except those no other member can fix, because the request
// itself is at fault or the caller stopped it:
//
//   - caller cancellation (context.Canceled);
//   - a request rejected locally (types.ErrInvalidModelConfig);
//   - a request the provider called invalid (types.ErrorKindInvalidRequest);
//   - an exhausted or busy budget (types.ErrBudgetExceeded, types.ErrBudgetBusy).
//
// Authentication, context-length, content-filter, rate-limit, and outage
// errors still fall back: another member can have other credentials, a larger
// context window, or a different safety policy. Callers that would rather
// compact the conversation on a context-length error can stop there with
// FallbackOn: func(err error) bool { return DefaultFallbackOn(err) && !types.IsContextLength(err) }.
func DefaultFallbackOn(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled),
		errors.Is(err, types.ErrInvalidModelConfig),
		errors.Is(err, types.ErrBudgetExceeded),
		errors.Is(err, types.ErrBudgetBusy),
		types.KindOf(err) == types.ErrorKindInvalidRequest:
		return false
	}
	return true
}

// New creates a provider that tries cfg.Providers in order. An empty or
// nil member is an error wrapping types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (*Provider, error) {
	for _, o := range opts {
		o(&cfg)
	}
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("%w: fallback: no providers", types.ErrInvalidConfig)
	}
	for i, p := range cfg.Providers {
		if p == nil {
			return nil, fmt.Errorf("%w: fallback: provider %d is nil", types.ErrInvalidConfig, i)
		}
	}
	cfg.Providers = append([]types.Provider(nil), cfg.Providers...)
	return &Provider{Config: cfg}, nil
}

// Of is New over providers with the default fallback rule.
func Of(providers ...types.Provider) (*Provider, error) {
	return New(Config{Providers: providers})
}

// derive returns a chain with the same rule over other members.
func (f *Provider) derive(providers []types.Provider) *Provider {
	return &Provider{Config: Config{Providers: providers, FallbackOn: f.FallbackOn}}
}

// Name implements types.NamedProvider.
func (f *Provider) Name() string { return "fallback" }

// Model implements types.ModelProvider. A fallback chain has no single model,
// so it reports the primary's: that is the model a request is served by unless
// something goes wrong, and it is what telemetry and cache keys want.
func (f *Provider) Model() string {
	if len(f.Providers) == 0 {
		return ""
	}
	return types.ProviderModel(f.Providers[0])
}

// Capabilities implements types.CapabilityReporter as the intersection over
// every member of the chain.
//
// The intersection is the only honest answer. Any member may serve the
// request, so a capability the primary has and the secondary lacks is not one
// a caller may rely on: promising it produces a chain that enforces a response
// schema until the primary goes down, then quietly stops. A member that
// reports nothing collapses the intersection to nothing, which is the same
// conservative direction. Capabilities that need request options are dropped
// when no member can receive them, since an options call skips such members.
func (f *Provider) Capabilities() types.ModelCapabilities {
	if len(f.Providers) == 0 {
		return types.ModelCapabilities{}
	}
	out, _ := types.ProviderCapabilities(f.Providers[0])
	for _, p := range f.Providers[1:] {
		next, _ := types.ProviderCapabilities(p)
		out = out.Intersect(next)
	}
	return optionscheck.Narrow(out, f.Providers...)
}

// WithTarget implements types.TargetSwitcher for a model target: each
// member is re-targeted, and a member that cannot be re-targeted is kept
// as-is. A profile or preset target is not defined on a fallback chain.
func (f *Provider) WithTarget(t types.Target) (types.Provider, error) {
	providers, err := types.RetargetMembers(f.Providers, t, "fallback")
	if err != nil {
		return nil, err
	}
	return f.derive(providers), nil
}

// Stream implements types.Provider. A request with a schema skips members
// that cannot enforce one, and a request with options skips members that
// cannot receive them, so an outage never downgrades schema-checked output to
// free-form text or drops a forced tool choice. A member is skipped when it
// does not accept the field, or when it does (as every decorator does) but
// rejects it because the provider it wraps cannot. When no member can serve
// the request, the call fails with a FallbackError whose errors match
// types.ErrInvalidModelConfig.
//
// Each member fits the request's parts to its own offering (its conversion
// decorator plans them per attempt). A member whose plan rejects a part
// never reached the network, so the next member is tried, whatever
// FallbackOn says.
func (f *Provider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema == nil && req.Options == nil {
		return f.streamOver(ctx, f.Providers, f.orModality(f.fallbackOn()), func(p types.Provider) (<-chan types.Delta, error) {
			return p.Stream(ctx, req)
		})
	}
	var members []types.Provider
	var skipped []error
	for _, p := range f.Providers {
		switch {
		case req.Options != nil && !types.AcceptsOptions(p):
			skipped = append(skipped, optionscheck.Unsupported(p))
		case req.Schema != nil && !types.AcceptsSchema(p):
			skipped = append(skipped, schemacheck.Unsupported(p, "provider cannot enforce a response schema"))
		default:
			members = append(members, p)
		}
	}
	if len(members) == 0 {
		return nil, &types.FallbackError{Errors: skipped}
	}
	// A rejected schema or options never reached the network, so the next
	// member can still serve the request even though the error is permanent.
	shouldFallback := f.orModality(func(err error) bool {
		return req.Schema != nil && schemacheck.IsUnsupported(err) ||
			req.Options != nil && optionscheck.IsUnsupported(err) || f.fallbackOn()(err)
	})
	return f.streamOver(ctx, members, shouldFallback, func(p types.Provider) (<-chan types.Delta, error) {
		return p.Stream(ctx, req)
	})
}

// SupportsSchema implements types.StructuredOutputProvider: a schema is
// served by the members that can enforce it.
func (f *Provider) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider: options are served by
// the members that can receive them.
func (f *Provider) SupportsOptions() bool { return true }

// Unwrap returns the members in order. See package wrapper.
func (f *Provider) Unwrap() []types.Provider {
	return append([]types.Provider(nil), f.Providers...)
}

// Close implements types.Closer by closing every member and joining their
// errors.
func (f *Provider) Close(ctx context.Context) error {
	var errs []error
	for _, p := range f.Providers {
		if err := types.CloseProvider(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fallbackOn returns the configured fallback predicate or the default.
func (f *Provider) fallbackOn() func(error) bool {
	if f.FallbackOn == nil {
		return DefaultFallbackOn
	}
	return f.FallbackOn
}

// orModality extends a fallback predicate to a member that rejected the
// request's parts before sending it.
func (f *Provider) orModality(next func(error) bool) func(error) bool {
	return func(err error) bool {
		return errors.Is(err, types.ErrModalityUnsupported) || next(err)
	}
}

// streamOver tries each provider in order until one returns a channel, then
// relays its deltas so mid-stream errors can still trigger fallback. If every
// provider fails before a channel is obtained, the accumulated FallbackError
// is returned directly (preserving the original synchronous contract).
func (f *Provider) streamOver(ctx context.Context, providers []types.Provider, shouldFallback func(error) bool, call func(types.Provider) (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	var errs []error
	for i, p := range providers {
		ch, err := call(p)
		if err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil || !shouldFallback(err) {
				break
			}
			continue
		}
		out := make(chan types.Delta)
		go f.relay(ctx, out, ch, providers[i+1:], call, shouldFallback, errs)
		return out, nil
	}

	return nil, &types.FallbackError{Errors: errs}
}

// isContentDelta reports whether d carries output the consumer has visibly
// received: anything that would duplicate on a retry with another provider.
// Only content-bearing deltas latch relay's no-fallback gate:
//
//   - UsageDelta does NOT latch. Anthropic's adapter emits a UsageDelta at
//     message_start, before any content block, so if it latched, a stream that
//     died in the most common failure window (request accepted, connection
//     dropped before content) could never fall back. The other adapters
//     (openai, google, ollama) emit usage only at stream end, after content.
//     If a UsageDelta was forwarded and we then fall back, the consumer sees
//     the failed provider's usage followed by the next provider's full stream;
//     that is acceptable: the aggregator merges usage (UsageDelta.Merge), and
//     the failed request's tokens were genuinely consumed.
//   - RouteDelta does NOT latch. Routers and splits send one before every
//     attempt, ahead of any output, so latching on it would make a wrapped
//     router or split unable to fall back at all. A consumer that sees a
//     failed member's route followed by the next member's route keeps the
//     last one.
//   - DoneDelta and ErrorDelta do NOT latch: terminal markers, not content.
//   - ConversionDelta does NOT latch: it reports what an attempt converted,
//     ahead of its output, and its cost is real even if the attempt fails.
//   - Everything else latches: PartStart/PartDelta/PartEnd,
//     ToolExec*, MarkerDelta, HandoffDelta, and any future delta type
//     (defaulting to latching is the safe direction: worst case we propagate
//     an error instead of silently duplicating output).
//
// This mirrors the retry package's isContentDelta.
func isContentDelta(d types.Delta) bool {
	switch d.(type) {
	case types.UsageDelta, types.RouteDelta, types.ConversionDelta, types.DoneDelta, types.ErrorDelta:
		return false
	default:
		return true
	}
}

// relay forwards deltas from src to out. An ErrorDelta that arrives before any
// content-bearing delta has been forwarded downstream discards the failed
// stream and retries with the remaining providers; anything later is
// propagated as-is. When the remaining providers are exhausted, the
// accumulated FallbackError is emitted as an ErrorDelta (the channel was
// already handed to the consumer).
//
// Every return path that abandons a live src must `go drain(src)` first:
// provider adapters may send on unbuffered channels, and a producer blocked on
// a send to an abandoned channel leaks forever.
func (f *Provider) relay(ctx context.Context, out chan<- types.Delta, src <-chan types.Delta, rest []types.Provider, call func(types.Provider) (<-chan types.Delta, error), shouldFallback func(error) bool, errs []error) {
	defer close(out)

	send := func(d types.Delta) bool {
		select {
		case out <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}

	forwarded := false
	for {
		var fbErr error
	read:
		for {
			select {
			case <-ctx.Done():
				go drain(src) // abandoning a live src: unblock its producer
				return
			case d, ok := <-src:
				if !ok {
					return // stream finished (cleanly, or after a propagated error)
				}
				if ed, isErr := d.(types.ErrorDelta); isErr && !forwarded && ctx.Err() == nil && shouldFallback(ed.Error) {
					fbErr = ed.Error
					break read
				}
				if !send(d) {
					go drain(src) // send failed on ctx.Done: src is still live
					return
				}
				if isContentDelta(d) {
					forwarded = true
				}
			}
		}

		// Abandon the failed stream (drained so its producer never blocks) and
		// move on to the next provider.
		go drain(src)
		errs = append(errs, fbErr)
		src = nil
		for len(rest) > 0 {
			p := rest[0]
			rest = rest[1:]
			ch, err := call(p)
			if err == nil {
				src = ch
				break
			}
			errs = append(errs, err)
			if ctx.Err() != nil || !shouldFallback(err) {
				break
			}
		}
		if src == nil {
			send(types.ErrorDelta{Error: &types.FallbackError{Errors: errs}})
			return
		}
	}
}

// drain discards the remainder of an abandoned stream.
func drain(ch <-chan types.Delta) {
	for range ch {
	}
}

func (f *Provider) NewSession() types.Provider {
	children := make([]types.Provider, len(f.Providers))
	for i, p := range f.Providers {
		children[i] = types.NewProviderSession(p)
	}
	return f.derive(children)
}
