package retry

import (
	"context"
	"math"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Config controls retry behavior.
type Config struct {
	MaxAttempts int              // total attempts (1 = no retry)
	BaseDelay   time.Duration    // initial delay between retries
	MaxDelay    time.Duration    // cap on delay
	Multiplier  float64          // backoff multiplier (default 2.0)
	ShouldRetry func(error) bool // nil = retry on IsTransient errors
}

// DefaultConfig returns sensible defaults: 3 attempts, 500ms base,
// 10s cap, 2x exponential backoff, transient-only.
func DefaultConfig() Config {
	return Config{
		MaxAttempts: 3,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Multiplier:  2.0,
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
	return &Provider{Inner: inner, Config: cfg}
}

func (r *Provider) Name() string {
	return "retry(" + types.ProviderName(r.Inner) + ")"
}

// Model implements types.ModelProvider by delegating to the inner provider.
func (r *Provider) Model() string { return types.ProviderModel(r.Inner) }

// WithModel implements types.ModelSwitcher: it re-targets the inner provider
// when it supports model switching, keeping the same retry config.
func (r *Provider) WithModel(model string) types.Provider {
	return &Provider{Inner: types.ProviderWithModel(r.Inner, model), Config: r.Config}
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
// fail closed still can.
func (r *Provider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(r.Inner)
	return caps
}

func (r *Provider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return r.retryLoop(ctx, func() (<-chan types.Delta, error) {
		return r.Inner.ChatStream(ctx, messages, tools)
	})
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
// If the inner provider supports structured output, retries use it.
// Otherwise, falls back to ChatStream (schema is lost).
func (r *Provider) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	if sp, ok := r.Inner.(types.StructuredOutputProvider); ok {
		return r.retryLoop(ctx, func() (<-chan types.Delta, error) {
			return sp.ChatStreamWithSchema(ctx, messages, tools, schema)
		})
	}
	return r.ChatStream(ctx, messages, tools)
}

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
			if !r.backoff(ctx, attempt) {
				return nil, ctx.Err()
			}
			continue
		}

		// The call succeeded synchronously; peek the leading deltas to detect a
		// channel-delivered error that arrives before any content.
		buffered, channelErr, hadContent := drainUntilContentOrError(ctx, ch)
		if channelErr == nil {
			// Stream produced content (or closed cleanly) without a leading error.
			return replay(buffered, nil, ch), nil
		}

		// A leading ErrorDelta was observed. If content already streamed we must
		// surface it (retrying would re-run a partially consumed turn), and if it
		// is not retryable we surface it too. Re-emit the ErrorDelta so downstream
		// consumers still see a terminal error event.
		lastErr = channelErr
		if hadContent || ctx.Err() != nil || !shouldRetry(channelErr) {
			return replay(buffered, channelErr, ch), nil
		}
		if !r.backoff(ctx, attempt) {
			return nil, ctx.Err()
		}
	}

	return nil, &types.RetryError{Attempts: r.Config.MaxAttempts, Last: lastErr}
}

// backoff sleeps before the next attempt using exponential backoff. It returns
// false if the context was cancelled while waiting. No sleep occurs after the
// final attempt.
func (r *Provider) backoff(ctx context.Context, attempt int) bool {
	if attempt >= r.Config.MaxAttempts-1 {
		return true
	}
	delay := time.Duration(float64(r.Config.BaseDelay) * math.Pow(r.Config.Multiplier, float64(attempt)))
	if delay > r.Config.MaxDelay {
		delay = r.Config.MaxDelay
	}
	select {
	case <-time.After(delay):
		return true
	case <-ctx.Done():
		return false
	}
}

// isContentDelta reports whether d carries model output (text, tool calls,
// thinking, tool execution). Metadata-only deltas (usage, done) do not count as
// content for retry purposes, so a usage preamble followed by an ErrorDelta is
// still retryable.
func isContentDelta(d types.Delta) bool {
	switch d.(type) {
	case types.UsageDelta, types.DoneDelta, types.ErrorDelta:
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
// terminal error event, then forwards the remainder of rest.
func replay(buffered []types.Delta, errAfter error, rest <-chan types.Delta) <-chan types.Delta {
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		for _, d := range buffered {
			out <- d
		}
		if errAfter != nil {
			out <- types.ErrorDelta{Error: errAfter}
		}
		for d := range rest {
			out <- d
		}
	}()
	return out
}
