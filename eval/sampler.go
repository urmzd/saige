package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Sampler configures repeated sampling of the non-deterministic parts of an
// evaluation.
//
// One run of an LLM judge, or of an LLM subject, is one draw from a
// distribution. A Sampler asks for N draws and reports how much they moved, so
// a suite can tell a stable 0.8 from a 0.8 that was 0.4 a moment ago.
//
// It applies in two places:
//
//   - Scorers: [Sampled] wraps one scorer, [WithSampler] wraps every scorer in
//     a [Run]. Each observation is scored N times; the reduced value becomes
//     [Score.Value] and the spread is reported in [Score.Samples].
//   - Subjects: [Sampler.Replicate] expands each observation into N copies, so
//     [Populate] runs the system under test N times per input.
//
// Deterministic scorers gain nothing from resampling. Mark one with
// [Deterministic] and [WithSampler] scores it once.
type Sampler struct {
	// N is the number of samples. Values below 2 sample once, which leaves
	// scoring exactly as it is without a Sampler.
	N int
	// Concurrency is how many samples run at once. Values below 2 run them
	// one after another, the safe default against provider rate limits.
	Concurrency int
	// Tolerance is the largest spread (max minus min) still reported as
	// stable. The zero value flags any change at all.
	Tolerance float64
	// Reduce combines the sampled values into [Score.Value]. Nil means [Mean].
	Reduce func(values []float64) float64
}

// SampleStats describes how a score behaved across samples.
type SampleStats struct {
	// Values holds each successful sample, in sample order.
	Values []float64 `json:"values"`
	// Reasons holds each successful sample's reason, in sample order, so a
	// changed verdict can be read next to the explanation that produced it.
	Reasons []string `json:"reasons,omitempty"`
	Mean    float64  `json:"mean"`
	StdDev  float64  `json:"std_dev"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	// Errors counts samples that failed. They are excluded from the
	// statistics rather than counted as zero.
	Errors int `json:"errors,omitempty"`
	// Stable reports whether Max minus Min is within the Sampler's Tolerance.
	Stable bool `json:"stable"`
}

// Mean is the default reducer.
func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// Median is a reducer that resists a single outlying sample.
func Median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// Min is the pessimistic reducer: a gate that must hold on every sample.
func Min(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	lowest := values[0]
	for _, v := range values[1:] {
		lowest = math.Min(lowest, v)
	}
	return lowest
}

// Replicate returns N copies of each observation, numbered 1..N in
// [Observation.Sample], so [Populate] runs the subject once per copy. IDs are
// kept, which lets results be grouped by ID to compare samples. With N below 2
// the observations are returned unchanged.
func (s Sampler) Replicate(observations []Observation) []Observation {
	if s.N < 2 {
		return observations
	}
	out := make([]Observation, 0, len(observations)*s.N)
	for _, obs := range observations {
		for n := 1; n <= s.N; n++ {
			sample := obs
			sample.Sample = n
			out = append(out, sample)
		}
	}
	return out
}

// deterministic is implemented by scorers that always return the same score
// for the same observation.
type deterministic interface{ Deterministic() bool }

type deterministicScorer struct{ Scorer }

func (deterministicScorer) Deterministic() bool { return true }

// Deterministic marks a scorer as returning the same score for the same
// observation every time, so [WithSampler] scores it once.
func Deterministic(s Scorer) Scorer { return deterministicScorer{s} }

type sampledScorer struct {
	inner   Scorer
	sampler Sampler
}

// Sampled wraps a scorer so each observation is scored sampler.N times. The
// returned [Score] carries the reduced value and a [SampleStats]. A sample
// that errors is counted and skipped; the score errors only when every sample
// does. With N below 2, or a scorer marked [Deterministic], the scorer is
// returned unchanged.
func Sampled(inner Scorer, sampler Sampler) Scorer {
	if sampler.N < 2 {
		return inner
	}
	if d, ok := inner.(deterministic); ok && d.Deterministic() {
		return inner
	}
	return &sampledScorer{inner: inner, sampler: sampler}
}

func (s *sampledScorer) Name() string { return s.inner.Name() }

func (s *sampledScorer) Score(ctx context.Context, obs Observation) (Score, error) {
	n := s.sampler.N
	scores := make([]Score, n)
	errs := make([]error, n)

	workers := max(s.sampler.Concurrency, 1)
	slots := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			scores[i], errs[i] = s.inner.Score(ctx, obs)
			if errs[i] == nil && scores[i].Error != "" {
				errs[i] = fmt.Errorf("%s", scores[i].Error)
			}
		}(i)
	}
	wg.Wait()

	stats := &SampleStats{}
	name := ""
	var lastErr error
	for i := range n {
		switch {
		case errs[i] != nil:
			stats.Errors++
			lastErr = errs[i]
		case scores[i].Name == "":
			// The scorer declined this observation; nothing to sample.
		default:
			name = scores[i].Name
			stats.Values = append(stats.Values, scores[i].Value)
			stats.Reasons = append(stats.Reasons, scores[i].Reason)
		}
	}
	if len(stats.Values) == 0 {
		if lastErr != nil {
			return Score{}, fmt.Errorf("all %d samples failed: %w", n, lastErr)
		}
		return Score{}, nil
	}

	stats.summarize(s.sampler.Tolerance)
	reduce := s.sampler.Reduce
	if reduce == nil {
		reduce = Mean
	}
	reason := fmt.Sprintf("%d samples, spread %.3g", len(stats.Values), stats.Max-stats.Min)
	if !stats.Stable {
		reason += " (unstable)"
	}
	return Score{Name: name, Value: reduce(stats.Values), Reason: reason, Samples: stats}, nil
}

func (st *SampleStats) summarize(tolerance float64) {
	st.Mean = Mean(st.Values)
	st.Min, st.Max = st.Values[0], st.Values[0]
	variance := 0.0
	for _, v := range st.Values {
		st.Min, st.Max = math.Min(st.Min, v), math.Max(st.Max, v)
		variance += (v - st.Mean) * (v - st.Mean)
	}
	st.StdDev = math.Sqrt(variance / float64(len(st.Values)))
	st.Stable = st.Max-st.Min <= tolerance
	if allEmpty(st.Reasons) {
		st.Reasons = nil
	}
}

func allEmpty(xs []string) bool {
	for _, x := range xs {
		if x != "" {
			return false
		}
	}
	return true
}
