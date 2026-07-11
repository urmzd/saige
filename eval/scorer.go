package eval

import "context"

// Scorer computes a named metric from an [Observation].
// Implementations must be safe for concurrent use.
//
// If the scorer is not applicable to a given observation (e.g., a RAG scorer
// given an agent observation with no RAG annotations), it should return a
// zero-value [Score] with an empty Name. The framework will skip it.
//
// A returned error is recorded on that observation's result as an errored
// [Score] (excluded from aggregates); it does not abort the suite.
type Scorer interface {
	Name() string
	Score(ctx context.Context, obs Observation) (Score, error)
}

// ScorerFunc adapts a plain function into a [Scorer].
type ScorerFunc struct {
	name string
	fn   func(ctx context.Context, obs Observation) (Score, error)
}

// NewScorerFunc creates a [Scorer] from a function.
func NewScorerFunc(name string, fn func(ctx context.Context, obs Observation) (Score, error)) *ScorerFunc {
	return &ScorerFunc{name: name, fn: fn}
}

func (s *ScorerFunc) Name() string { return s.name }

func (s *ScorerFunc) Score(ctx context.Context, obs Observation) (Score, error) {
	return s.fn(ctx, obs)
}

// Aggregate computes the mean of each unique score name across results.
// Errored scores are excluded rather than counted as zero.
func Aggregate(results []ObservationResult) map[string]float64 {
	sums := make(map[string]float64)
	counts := make(map[string]int)

	for _, r := range results {
		for _, s := range r.Scores {
			if s.Name == "" || s.Error != "" {
				continue
			}
			sums[s.Name] += s.Value
			counts[s.Name]++
		}
	}

	agg := make(map[string]float64, len(sums))
	for name, sum := range sums {
		agg[name] = sum / float64(counts[name])
	}
	return agg
}
