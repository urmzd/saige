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
// completes without an ErrorDelta, was not cancelled, and did not stop at the
// output token limit. done, when non-nil, runs after the store attempt with
// whether the recording was stored. Tool-call streams are
// cacheable (a tool call is a pure function of the input): TextStart/Content/End,
// Thinking*, and ToolCall* deltas are recorded. UsageDelta is captured
// separately. DoneDelta / MarkerDelta / ToolExec* / ErrorDelta are not recorded.
//
// The recording is also dropped when the attempt that served did not send
// the view the key was planned for: a conversion that fell back to another
// action, or a failover to a member that planned differently, changes the
// executed report (the last types.ConversionDelta), and the response then
// belongs to another key.
func (p *Provider) recordAndTee(ctx context.Context, key string, planned view, in <-chan types.Delta, done func(stored bool)) <-chan types.Delta {
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)
		stored := false
		if done != nil {
			defer func() { done(stored) }()
		}

		var rec CachedResponse
		failed := false
		openBlocks := 0
		served := view{}

		for d := range in {
			switch v := d.(type) {
			case types.PartStart:
				openBlocks++
				if v.Kind == types.KindToolCall && !p.cfg.CacheToolCalls {
					failed = true
				}
			case types.PartEnd:
				openBlocks--
			}
			if openBlocks < 0 {
				failed = true
			}
			switch v := d.(type) {
			case types.RouteDelta:
				served = view{} // a new attempt: its own report follows
			case types.ConversionDelta:
				served = view{hash: v.Report.Hash, converts: true}
			case types.ErrorDelta:
				failed = true // poison the recording; do not cache
			case types.UsageDelta:
				v.FinishReasons = append([]string(nil), v.FinishReasons...)
				for _, reason := range v.FinishReasons {
					// A response cut at the output limit is incomplete, even
					// when its blocks are balanced.
					if types.IsTruncationFinishReason(reason) {
						failed = true
					}
				}
				rec.Usage = rec.Usage.Merge(v) // providers may emit usage in parts
			default:
				if !recordable(v) {
					break // DoneDelta / MarkerDelta / ToolExec* / others: forwarded, not recorded.
				}
				cloned, err := cloneDelta(v)
				if err != nil {
					failed = true
				} else {
					rec.Deltas = append(rec.Deltas, cloned)
				}
			}
			// Always forward to the live consumer.
			select {
			case out <- d:
			case <-ctx.Done():
				failed = true // partial; abandon recording
			}
		}

		if served.converts != planned.converts || (planned.converts && served.hash != planned.hash) {
			failed = true // the served view is not the one the key names
		}
		if failed || openBlocks != 0 || ctx.Err() != nil || len(rec.Deltas) == 0 {
			return // correctness: never cache error/partial/empty streams
		}
		if err := p.cfg.Cache.Set(ctx, key, rec, p.cfg.TTL); err != nil {
			p.cfg.Logger.Warn("response cache set failed", "error", err)
			return
		}
		stored = true
	}()
	return out
}

// replay regenerates the exact provider-shaped stream the agent loop expects:
// the recorded content deltas followed by a final UsageDelta marked CacheHit.
// The provider stream does not emit DoneDelta (that is the agent's EventStream),
// so replay mirrors a provider, not agent.Replay.
//
// Every replayed tool call gets a fresh ID, and argument, end, and citation
// deltas that named the recorded ID are rewritten to match, so parallel calls
// still pair with their own arguments.
func replay(cr CachedResponse) <-chan types.Delta {
	out := make(chan types.Delta, len(cr.Deltas)+1)
	ids := map[string]string{}
	remap := func(id string) string {
		if fresh, ok := ids[id]; ok {
			return fresh
		}
		return id
	}
	for _, d := range cr.Deltas {
		cloned, err := cloneDelta(d)
		if err != nil {
			out <- types.ErrorDelta{Error: err}
			close(out)
			return out
		}
		switch v := cloned.(type) {
		case types.PartStart:
			if v.Kind == types.KindToolCall {
				fresh := types.NewID()
				if v.ID != "" {
					ids[v.ID] = fresh
				}
				v.ID = fresh
				cloned = v
			}
		case types.PartEnd:
			if tc, ok := v.Part.(types.ToolCallPart); ok {
				tc.ID = remap(tc.ID)
				v.Part = tc
				cloned = v
			}
		case types.CitationDelta:
			v.ToolCallID = remap(v.ToolCallID)
			cloned = v
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
	case types.PartEnd:
		if value.Part == nil {
			return value, nil
		}
		// A round trip through the part codec deep-copies the part, with
		// numbers as json.Number, as a byte store returns them.
		raw, err := types.MarshalPartInline(value.Part)
		if err != nil {
			return nil, err
		}
		p, err := types.UnmarshalRolePart[types.AssistantPart](raw)
		value.Part = p
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
