package fallback

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// Provider tries providers in order, falling back on failure.
// By default it falls back on any error. Set FallbackOn to control
// which errors trigger fallback (e.g. types.IsTransient for transient-only).
//
// Fallback covers both an immediate ChatStream error and a mid-stream error
// (an ErrorDelta) that arrives before any content-bearing delta has been
// delivered downstream (see isContentDelta). Once content has been forwarded,
// falling back would duplicate partial output, so the error delta is
// propagated as-is instead.
type Provider struct {
	Providers  []types.Provider
	FallbackOn func(error) bool // nil = fallback on any error
}

// New creates a provider that tries each in order.
func New(providers ...types.Provider) *Provider {
	return &Provider{Providers: providers}
}

func (f *Provider) Name() string { return "fallback" }

// WithModel implements types.ModelSwitcher. It returns a new fallback provider
// whose children each target the given model (children that do not implement
// types.ModelSwitcher are kept as-is). This lets ConfigContent.Model switching
// propagate through fallback-wrapped deployments.
func (f *Provider) WithModel(model string) types.Provider {
	providers := make([]types.Provider, len(f.Providers))
	for i, p := range f.Providers {
		providers[i] = types.ProviderWithModel(p, model)
	}
	return &Provider{Providers: providers, FallbackOn: f.FallbackOn}
}

func (f *Provider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return f.stream(ctx, func(p types.Provider) (<-chan types.Delta, error) {
		return p.ChatStream(ctx, messages, tools)
	})
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
// For each provider, it tries ChatStreamWithSchema if the provider supports it,
// otherwise falls back to ChatStream.
func (f *Provider) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	return f.stream(ctx, func(p types.Provider) (<-chan types.Delta, error) {
		if sp, ok := p.(types.StructuredOutputProvider); ok {
			return sp.ChatStreamWithSchema(ctx, messages, tools, schema)
		}
		return p.ChatStream(ctx, messages, tools)
	})
}

// stream tries each provider in order until one returns a channel, then relays
// its deltas so mid-stream errors can still trigger fallback. If every provider
// fails before a channel is obtained, the accumulated FallbackError is returned
// directly (preserving the original synchronous contract).
func (f *Provider) stream(ctx context.Context, call func(types.Provider) (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	shouldFallback := f.FallbackOn
	if shouldFallback == nil {
		shouldFallback = func(error) bool { return true }
	}

	var errs []error
	for i, p := range f.Providers {
		ch, err := call(p)
		if err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil || !shouldFallback(err) {
				break
			}
			continue
		}
		out := make(chan types.Delta)
		go f.relay(ctx, out, ch, f.Providers[i+1:], call, shouldFallback, errs)
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
//   - DoneDelta and ErrorDelta do NOT latch: terminal markers, not content.
//   - Everything else latches: Text*/Thinking*/ToolCall* start/content/end,
//     ToolExec*, MarkerDelta, HandoffDelta, and any future delta type
//     (defaulting to latching is the safe direction: worst case we propagate
//     an error instead of silently duplicating output).
//
// This mirrors the retry package's isContentDelta.
func isContentDelta(d types.Delta) bool {
	switch d.(type) {
	case types.UsageDelta, types.DoneDelta, types.ErrorDelta:
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
