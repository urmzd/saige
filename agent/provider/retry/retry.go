package retry

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
	"github.com/urmzd/saige/agent/types"
)

// Config controls retry behavior.
type Config struct {
	MaxAttempts int              // total attempts (1 = no retry)
	BaseDelay   time.Duration    // initial delay between retries
	MaxDelay    time.Duration    // cap on delay
	Multiplier  float64          // backoff multiplier (default 2.0)
	ShouldRetry func(error) bool // nil = retry on IsTransient errors

	// DisableJitter turns off full jitter. With jitter (the default) each
	// wait is drawn uniformly from [0, computed backoff], so concurrent
	// callers that share a rate-limited key do not retry in lockstep.
	DisableJitter bool
	// MaxRetryAfter caps how long a provider's Retry-After may stretch one
	// wait. A delay the provider asked for is honored up to this cap, even
	// past MaxDelay. Zero uses 60s.
	MaxRetryAfter time.Duration
}

// DefaultConfig returns sensible defaults: 3 attempts, 500ms base,
// 10s cap, 2x exponential backoff with full jitter, transient-only, and
// Retry-After honored up to 60s.
func DefaultConfig() Config {
	return Config{
		MaxAttempts:   3,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      10 * time.Second,
		Multiplier:    2.0,
		MaxRetryAfter: 60 * time.Second,
	}
}

// Provider wraps a Provider with retry logic and exponential backoff.
type Provider struct {
	Inner  types.Provider
	Config Config
}

// New wraps a provider with the given retry config.
func New(inner types.Provider, cfg Config) *Provider {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.Multiplier <= 0 {
		cfg.Multiplier = 2.0
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 500 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 10 * time.Second
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = 60 * time.Second
	}
	return &Provider{Inner: inner, Config: cfg}
}

func (r *Provider) Name() string {
	return "retry(" + types.NameOf(r.Inner) + ")"
}

// Model implements types.ModelProvider by delegating to the inner provider.
func (r *Provider) Model() string { return types.ProviderModel(r.Inner) }

// WithModel implements types.ModelSwitcher: it re-targets the inner provider
// when it supports model switching, keeping the same retry config.
func (r *Provider) WithModel(model string) types.Provider {
	return &Provider{Inner: types.ProviderWithModel(r.Inner, model), Config: r.Config}
}

// WithTarget implements types.TargetSwitcher: it re-targets the inner
// provider, keeping the same retry config.
func (r *Provider) WithTarget(t types.Target) (types.Provider, error) {
	inner, err := types.ProviderWithTarget(r.Inner, t)
	if err != nil {
		return nil, err
	}
	return &Provider{Inner: inner, Config: r.Config}, nil
}

// ContentSupport implements types.ContentNegotiator by delegating to the inner
// provider. Without this the file pipeline sees a retry-wrapped adapter as
// supporting no media at all and extracts every attachment to text, silently
// discarding images the model could have read natively.
func (r *Provider) ContentSupport() types.ContentSupport {
	return types.ProviderContentSupport(r.Inner)
}

// Capabilities implements types.CapabilityReporter by delegating to the inner
// provider. When the inner provider does not report, the zero value is
// returned: it declares nothing and has Known false, so a caller that must
// fail closed still can. Capabilities that need request options are dropped
// when the inner provider cannot receive them.
func (r *Provider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(r.Inner)
	return optionscheck.Narrow(caps, r.Inner)
}

// Stream implements types.Provider. A schema or options the inner provider
// cannot receive are rejected with types.ErrInvalidModelConfig rather than
// silently dropped: the caller asked for them and must not receive output
// made without them.
func (r *Provider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil && !types.AcceptsSchema(r.Inner) {
		return nil, schemacheck.Unsupported(r.Inner, "provider cannot enforce a response schema")
	}
	if req.Options != nil && !types.AcceptsOptions(r.Inner) {
		return nil, optionscheck.Unsupported(r.Inner)
	}
	return r.retryLoop(ctx, func() (<-chan types.Delta, error) {
		return r.Inner.Stream(ctx, req)
	})
}

// SupportsSchema implements types.StructuredOutputProvider. A schema the
// inner provider cannot enforce is rejected when the request arrives.
func (r *Provider) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider. Options the inner
// provider cannot receive are rejected when the request arrives.
func (r *Provider) SupportsOptions() bool { return true }

// Unwrap returns the inner provider. See package wrapper.
func (r *Provider) Unwrap() types.Provider { return r.Inner }

// Close implements types.Closer by closing the inner provider.
func (r *Provider) Close() error { return types.CloseProvider(r.Inner) }

// retryLoop runs the call function with exponential backoff.
//
// It retries in two situations:
//  1. The call itself returns an error (e.g. a synchronous dial failure).
//  2. The returned stream emits an ErrorDelta BEFORE any content delta. Many
//     streaming adapters surface transient failures (529 overload, mid-handshake
//     timeouts) as an ErrorDelta on the channel rather than a synchronous error.
//     We buffer the leading deltas until either content arrives or the stream
//     errors, so the failure can be classified for retryability without losing
//     output. Once any content delta has streamed, an error is surfaced as-is
//     (we never retry a partially-consumed response).
func (r *Provider) retryLoop(ctx context.Context, call func() (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	shouldRetry := r.Config.ShouldRetry
	if shouldRetry == nil {
		shouldRetry = types.IsTransient
	}

	var lastErr error
	for attempt := range r.Config.MaxAttempts {
		ch, err := call()
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, lastErr
			}
			if !shouldRetry(err) {
				return nil, lastErr
			}
			if !r.backoff(ctx, attempt, err) {
				return nil, ctx.Err()
			}
			continue
		}

		// The call succeeded synchronously; peek the leading deltas to detect a
		// channel-delivered error that arrives before any content.
		buffered, channelErr, hadContent := drainUntilContentOrError(ctx, ch)
		if channelErr == nil {
			// Stream produced content (or closed cleanly) without a leading error.
			return replay(ctx, buffered, nil, ch), nil
		}

		// A leading ErrorDelta was observed. If content already streamed we must
		// surface it (retrying would re-run a partially consumed turn), and if it
		// is not retryable we surface it too. Re-emit the ErrorDelta so downstream
		// consumers still see a terminal error event.
		lastErr = channelErr
		if hadContent || ctx.Err() != nil || !shouldRetry(channelErr) {
			return replay(ctx, buffered, channelErr, ch), nil
		}
		// Abandon the failed attempt. A producer that keeps sending after its
		// ErrorDelta would otherwise block forever on the dropped channel.
		go drain(ch)
		if !r.backoff(ctx, attempt, channelErr) {
			return nil, ctx.Err()
		}
	}

	return nil, &types.RetryError{Attempts: r.Config.MaxAttempts, Last: lastErr}
}

// backoff sleeps before the next attempt. It returns false if the context was
// cancelled while waiting. No sleep occurs after the final attempt.
func (r *Provider) backoff(ctx context.Context, attempt int, err error) bool {
	if attempt >= r.Config.MaxAttempts-1 {
		return true
	}
	timer := time.NewTimer(r.delay(attempt, err, rand.Float64())) //nolint:gosec // jitter needs no cryptographic randomness
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// delay computes the wait before the attempt after attempt. See
// Config.Delay.
func (r *Provider) delay(attempt int, err error, u float64) time.Duration {
	return r.Config.Delay(attempt, err, u)
}

// Delay computes the wait before the attempt after attempt (0-based) failed
// with err. The exponential backoff, BaseDelay*Multiplier^attempt, is capped
// at MaxDelay and, unless DisableJitter is set, scaled by u (uniform in
// [0, 1)) for full jitter. A Retry-After carried by err is a floor on the
// result, clamped to MaxRetryAfter (60s when zero): the provider said how
// long to wait, so jitter never shortens it. Other retry loops, such as the
// embedding retrier, use it so every loop backs off the same way.
func (c Config) Delay(attempt int, err error, u float64) time.Duration {
	d := time.Duration(float64(c.BaseDelay) * math.Pow(c.Multiplier, float64(attempt)))
	if d > c.MaxDelay || d < 0 {
		d = c.MaxDelay
	}
	if !c.DisableJitter {
		d = time.Duration(float64(d) * u)
	}
	maxAfter := c.MaxRetryAfter
	if maxAfter <= 0 {
		maxAfter = 60 * time.Second
	}
	if after := min(types.RetryAfter(err), maxAfter); after > d {
		d = after
	}
	return d
}

// isContentDelta reports whether d carries model output (text, tool calls,
// thinking, tool execution). Metadata-only deltas (usage, route, done) do not
// count as content for retry purposes, so a usage or route preamble followed
// by an ErrorDelta is still retryable. Routers and splits send a RouteDelta
// before every attempt; the deltas buffered from a failed attempt are
// discarded, so a retried attempt reports only its own route.
func isContentDelta(d types.Delta) bool {
	switch d.(type) {
	case types.UsageDelta, types.RouteDelta, types.ConversionDelta, types.DoneDelta, types.ErrorDelta:
		return false
	default:
		return true
	}
}

// drainUntilContentOrError reads from ch until it observes either a content
// delta, an ErrorDelta, or the channel closes. It returns the deltas consumed
// so far (to be replayed), the error if one was seen before content, and
// whether any content delta was observed. Metadata deltas (usage) seen before
// the decision point are buffered and replayed regardless of the outcome.
func drainUntilContentOrError(ctx context.Context, ch <-chan types.Delta) (buffered []types.Delta, channelErr error, hadContent bool) {
	for {
		select {
		case d, ok := <-ch:
			if !ok {
				return buffered, nil, hadContent
			}
			if ed, isErr := d.(types.ErrorDelta); isErr {
				// Do not buffer the error delta itself; the caller decides whether
				// to retry or to surface it. On surface we re-emit it via replay.
				return buffered, ed.Error, hadContent
			}
			buffered = append(buffered, d)
			if isContentDelta(d) {
				return buffered, nil, true
			}
		case <-ctx.Done():
			return buffered, nil, hadContent
		}
	}
}

// replay returns a channel that first yields the buffered deltas, then re-emits
// errAfter (if non-nil) as an ErrorDelta so downstream consumers still see a
// terminal error event, then forwards the remainder of rest. Sends give up
// when ctx is done; rest is then drained so its producer never blocks.
func replay(ctx context.Context, buffered []types.Delta, errAfter error, rest <-chan types.Delta) <-chan types.Delta {
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		send := func(d types.Delta) bool {
			select {
			case out <- d:
				return true
			case <-ctx.Done():
				go drain(rest)
				return false
			}
		}
		for _, d := range buffered {
			if !send(d) {
				return
			}
		}
		if errAfter != nil && !send(types.ErrorDelta{Error: errAfter}) {
			return
		}
		for {
			select {
			case d, ok := <-rest:
				if !ok {
					return
				}
				if !send(d) {
					return
				}
			case <-ctx.Done():
				go drain(rest)
				return
			}
		}
	}()
	return out
}

// drain discards the remainder of an abandoned stream.
func drain(ch <-chan types.Delta) {
	for range ch {
	}
}

func (p *Provider) NewSession() types.Provider {
	return &Provider{Inner: types.NewProviderSession(p.Inner), Config: p.Config}
}

// EffectiveOptions implements types.OptionsReporter by forwarding to the inner
// provider. A retry replays the identical adapter, so every attempt sends
// these options.
func (r *Provider) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(r.Inner)
	return o
}
