package eval

import (
	"context"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

type evaluationUsageKey struct{}
type usageRecorder struct {
	mu          sync.Mutex
	timing      ObservationTiming
	calls       int
	unknownCost bool
}

// RecordEvaluationUsage records one judge request using the provider's existing
// usage and pricing metadata. known is false when the stream reported no usage.
// Unknown cost remains unknown, including when only some requests were priced.
// Outside a running evaluation it is a no-op. Concurrent samples may share ctx.
func RecordEvaluationUsage(ctx context.Context, usage types.UsageDelta, pricing types.Pricing, known bool) {
	recorder, ok := ctx.Value(evaluationUsageKey{}).(*usageRecorder)
	if !ok {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.calls++
	recorder.timing.InputTokens += usage.PromptTokens
	recorder.timing.OutputTokens += usage.CompletionTokens
	billed := types.UsageFromDelta(usage)
	if !known || (!usage.CacheHit && pricing.Unpriced(billed)) || (pricing.Currency != "" && pricing.Currency != "USD") {
		recorder.unknownCost = true
		return
	}
	if recorder.timing.CostUSD == nil {
		recorder.timing.SetCostUSD(0)
	}
	*recorder.timing.CostUSD += pricing.Cost(billed).Float()
}

func measureScore(ctx context.Context, s Scorer, obs Observation) (Score, error) {
	recorder := &usageRecorder{}
	start := time.Now()
	score, err := s.Score(context.WithValue(ctx, evaluationUsageKey{}, recorder), obs)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.calls > 0 {
		score.Timing = recorder.timing
		if recorder.unknownCost {
			score.Timing.CostUSD = nil
		}
	} else if d, ok := s.(deterministic); ok && d.Deterministic() && score.Timing.CostUSD == nil {
		score.Timing.SetCostUSD(0)
	}
	score.Timing.TotalMs = max(1, time.Since(start).Milliseconds())
	return score, err
}

func cloneTiming(t ObservationTiming) ObservationTiming {
	if t.CostUSD != nil {
		t.SetCostUSD(*t.CostUSD)
	}
	return t
}

func accumulateTiming(total *ObservationTiming, part ObservationTiming) {
	if part.TotalMs == 0 {
		return
	}
	first := total.TotalMs == 0
	total.TotalMs += part.TotalMs
	total.InputTokens += part.InputTokens
	total.OutputTokens += part.OutputTokens
	if first {
		total.CostUSD = cloneTiming(part).CostUSD
		return
	}
	if total.CostUSD == nil || part.CostUSD == nil {
		total.CostUSD = nil
		return
	}
	*total.CostUSD += *part.CostUSD
}
func evaluationUsage(results []ObservationResult) ObservationTiming {
	var timing ObservationTiming
	for _, r := range results {
		if r.EvaluationTiming.TotalMs > 0 {
			accumulateTiming(&timing, r.EvaluationTiming)
			continue
		}
		for _, s := range r.Scores {
			accumulateTiming(&timing, s.Timing)
		}
	}
	return timing
}

func measurementRevision(name string) string { return RevisionTag("measurement/v1", name) }

func appendMeasurements(obs Observation, scores []Score, usage ObservationTiming) []Score {
	have := map[string]bool{}
	for _, s := range scores {
		have[s.Name] = true
	}
	add := func(name string, value float64) {
		if !have[name] {
			scores = append(scores, Score{Name: name, Revision: measurementRevision(name), Value: value})
			have[name] = true
		}
	}
	t := obs.Timing
	if t.TotalMs > 0 {
		add("latency_ms", float64(t.TotalMs))
	}
	if t.TTFTMs > 0 {
		add("ttft_ms", float64(t.TTFTMs))
	}
	if t.TTLTMs > 0 {
		add("ttlt_ms", float64(t.TTLTMs))
	}
	if t.MedianITL > 0 {
		add("median_itl_ms", t.MedianITL)
	}
	if t.CostUSD != nil {
		add("cost_usd", *t.CostUSD)
	}
	if t.InputTokens > 0 {
		add("input_tokens", float64(t.InputTokens))
	}
	if t.OutputTokens > 0 {
		add("output_tokens", float64(t.OutputTokens))
	}
	if n, ok := t.TotalTokens(); ok {
		add("total_tokens", float64(n))
	}
	if usage.TotalMs == 0 {
		usage = evaluationUsage([]ObservationResult{{Scores: scores}})
	}
	if usage.TotalMs > 0 {
		add("evaluation_latency_ms", float64(usage.TotalMs))
	}
	if usage.CostUSD != nil {
		add("evaluation_cost_usd", *usage.CostUSD)
	}
	if n, ok := usage.TotalTokens(); ok {
		add("evaluation_tokens", float64(n))
	}
	return scores
}

func populateMeasured(ctx context.Context, subject Subject, obs *Observation, enabled bool) error {
	start := time.Now()
	err := subject(ctx, obs)
	if enabled && obs.Timing.TotalMs == 0 {
		obs.Timing.TotalMs = max(1, time.Since(start).Milliseconds())
	}
	return err
}
