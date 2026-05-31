package cache

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// CachedResponse is the fully-recorded, successful delta sequence for one call,
// minus terminal/transient deltas that are regenerated on replay.
type CachedResponse struct {
	Deltas []types.Delta    // ordered content deltas (text/tool/thinking)
	Usage  types.UsageDelta // the provider's reported usage at record time
}

// recordAndTee forwards every delta to the consumer unchanged, accumulating a
// recording. The recording is written to the cache ONLY when the stream
// completes without an ErrorDelta and was not cancelled. Tool-call streams are
// cacheable (a tool call is a pure function of the input): TextStart/Content/End,
// Thinking*, and ToolCall* deltas are recorded. UsageDelta is captured
// separately. DoneDelta / MarkerDelta / ToolExec* / ErrorDelta are not recorded.
func (p *Provider) recordAndTee(ctx context.Context, key string, in <-chan types.Delta) <-chan types.Delta {
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		var rec CachedResponse
		failed := false

		for d := range in {
			switch v := d.(type) {
			case types.ErrorDelta:
				failed = true // poison the recording; do not cache
			case types.UsageDelta:
				rec.Usage = rec.Usage.Merge(v) // providers may emit usage in parts
			case types.TextStartDelta, types.TextContentDelta, types.TextEndDelta,
				types.ThinkingStartDelta, types.ThinkingContentDelta, types.ThinkingEndDelta,
				types.ToolCallStartDelta, types.ToolCallArgumentDelta, types.ToolCallEndDelta:
				rec.Deltas = append(rec.Deltas, v)
			default:
				// DoneDelta / MarkerDelta / ToolExec* / others: forwarded, not recorded.
			}
			// Always forward to the live consumer.
			select {
			case out <- d:
			case <-ctx.Done():
				failed = true // partial; abandon recording
			}
		}

		if failed || ctx.Err() != nil || len(rec.Deltas) == 0 {
			return // correctness: never cache error/partial/empty streams
		}
		if err := p.cfg.Cache.Set(ctx, key, rec, p.cfg.TTL); err != nil {
			p.cfg.Logger.Warn("response cache set failed", "error", err)
		}
	}()
	return out
}

// replay regenerates the exact provider-shaped stream the agent loop expects:
// the recorded content deltas followed by a final UsageDelta marked CacheHit.
// The provider stream does not emit DoneDelta (that is the agent's EventStream),
// so replay mirrors a provider, not agent.Replay.
func replay(cr CachedResponse) <-chan types.Delta {
	out := make(chan types.Delta, len(cr.Deltas)+1)
	for _, d := range cr.Deltas {
		out <- d
	}
	u := cr.Usage
	u.CacheHit = true
	out <- u
	close(out)
	return out
}
