package eval

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"sort"
	"sync"

	"github.com/urmzd/saige/eval/analysis"
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
	// Confidence is the interval confidence level; zero uses 95%.
	Confidence float64
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
	Revision string `json:"revision,omitempty"`
	// Interval estimates the mean of successful repeated evaluations.
	// It is independent of the configured reducer. N excludes errors.
	Interval analysis.Stat `json:"interval"`
	// Confidence records the level used by Interval.
	Confidence float64 `json:"confidence"`
	// Expected includes failed and declined attempts.
	Expected int `json:"expected,omitempty"`
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
	// A sampled score is also unstable when any of its samples reported
	// itself unstable, such as a pairwise judge that contradicted itself
	// across the position swap; that sample's reason says why.
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
func (s deterministicScorer) Revision() string  { return ScorerRevision(s.Scorer) }

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
func (s *sampledScorer) Revision() string {
	reducer := "mean"
	if s.sampler.Reduce != nil {
		reducer = runtime.FuncForPC(reflect.ValueOf(s.sampler.Reduce).Pointer()).Name()
	}
	return RevisionTag("sampled/v1", ScorerRevision(s.inner), s.sampler.N, s.sampler.Tolerance, s.sampler.Confidence, reducer)
}

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

	stats := &SampleStats{Revision: s.Revision(), Expected: n, Confidence: s.sampler.Confidence}
	name := ""
	innerUnstable := 0
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
			if inner := scores[i].Samples; inner != nil && !inner.Stable {
				innerUnstable++
			}
		}
	}
	if len(stats.Values) == 0 {
		if lastErr != nil {
			return Score{}, fmt.Errorf("all %d samples failed: %w", n, lastErr)
		}
		return Score{}, nil
	}

	stats.summarize(s.sampler.Tolerance)
	if innerUnstable > 0 {
		stats.Stable = false
	}
	reduce := s.sampler.Reduce
	if reduce == nil {
		reduce = Mean
	}
	reason := fmt.Sprintf("%d samples, spread %.3g", len(stats.Values), stats.Max-stats.Min)
	if innerUnstable > 0 {
		reason += fmt.Sprintf(", %d inconsistent", innerUnstable)
	}
	if !stats.Stable {
		reason += " (unstable)"
	}
	return Score{Name: name, Revision: s.Revision(), Value: reduce(stats.Values), Reason: reason, Samples: stats}, nil
}

func (st *SampleStats) summarize(tolerance float64) {
	if st.Confidence <= 0 || st.Confidence >= 1 || math.IsNaN(st.Confidence) {
		st.Confidence = analysis.DefaultConfidence
	}
	expected := st.Expected
	if expected == 0 {
		expected = len(st.Values) + st.Errors
	}
	st.Interval = analysis.MeanStat(st.Values, expected, st.Confidence)
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
