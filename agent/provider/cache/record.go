package cache

import (
	"bytes"
	"context"
	"encoding/json"

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
		openBlocks := 0

		for d := range in {
			switch d.(type) {
			case types.TextStartDelta, types.ThinkingStartDelta, types.ToolCallStartDelta:
				openBlocks++
			case types.TextEndDelta, types.ThinkingEndDelta, types.ToolCallEndDelta:
				openBlocks--
			}
			if openBlocks < 0 {
				failed = true
			}
			if _, ok := d.(types.ToolCallStartDelta); ok && !p.cfg.CacheToolCalls {
				failed = true
			}
			switch v := d.(type) {
			case types.ErrorDelta:
				failed = true // poison the recording; do not cache
			case types.UsageDelta:
				v.FinishReasons = append([]string(nil), v.FinishReasons...)
				rec.Usage = rec.Usage.Merge(v) // providers may emit usage in parts
			case types.TextStartDelta, types.TextContentDelta, types.TextEndDelta,
				types.ThinkingStartDelta, types.ThinkingContentDelta, types.ThinkingEndDelta,
				types.ToolCallStartDelta, types.ToolCallArgumentDelta, types.ToolCallEndDelta, types.CitationDelta:
				cloned, err := cloneDelta(v)
				if err != nil {
					failed = true
				} else {
					rec.Deltas = append(rec.Deltas, cloned)
				}
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

		if failed || openBlocks != 0 || ctx.Err() != nil || len(rec.Deltas) == 0 {
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
		cloned, err := cloneDelta(d)
		if err != nil {
			out <- types.ErrorDelta{Error: err}
			close(out)
			return out
		}
		if call, ok := cloned.(types.ToolCallStartDelta); ok {
			call.ID = types.NewID()
			cloned = call
		}
		out <- cloned
	}
	u := cr.Usage
	u.FinishReasons = append([]string(nil), u.FinishReasons...)
	u.CacheHit = true
	out <- u
	close(out)
	return out
}

// Mutable payloads are copied at record and replay boundaries. The cache never
// lends an argument map to a tool or a citation map to a consumer.
func cloneDelta(delta types.Delta) (types.Delta, error) {
	switch value := delta.(type) {
	case types.ToolCallEndDelta:
		raw, err := json.Marshal(value.Arguments)
		if err != nil {
			return nil, err
		}
		value.Arguments = nil
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(&value.Arguments)
		return value, err
	case types.CitationDelta:
		raw, err := json.Marshal(value.Citation.Meta)
		if err != nil {
			return nil, err
		}
		value.Citation.Meta = nil
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(&value.Citation.Meta)
		return value, err
	default:
		return delta, nil
	}
}
