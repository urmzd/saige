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
	name    string
	renamed bool
	fn      func(ctx context.Context, obs Observation) (Score, error)
}

// NewScorerFunc creates a [Scorer] from a function.
func NewScorerFunc(name string, fn func(ctx context.Context, obs Observation) (Score, error)) *ScorerFunc {
	return &ScorerFunc{name: name, fn: fn}
}

func (s *ScorerFunc) Name() string { return s.name }

func (s *ScorerFunc) Score(ctx context.Context, obs Observation) (Score, error) {
	score, err := s.fn(ctx, obs)
	if s.renamed && score.Name != "" {
		score.Name = s.name
	}
	return score, err
}

// WithName returns a copy of the scorer that reports its metric under name,
// overriding the name the function sets on its scores.
func (s *ScorerFunc) WithName(name string) *ScorerFunc {
	return &ScorerFunc{name: name, renamed: true, fn: s.fn}
}

// Named returns s reporting its metric under name, so two configurations of
// one built-in scorer, such as two [TokenBudgetScorer] limits, report
// separate metrics. A declined observation stays declined, and a scorer
// marked [Deterministic] stays deterministic.
func Named(s Scorer, name string) Scorer {
	return &namedScorer{inner: s, name: name}
}

type namedScorer struct {
	inner Scorer
	name  string
}

func (n *namedScorer) Name() string { return n.name }

func (n *namedScorer) Score(ctx context.Context, obs Observation) (Score, error) {
	score, err := n.inner.Score(ctx, obs)
	if score.Name != "" {
		score.Name = n.name
	}
	return score, err
}

func (n *namedScorer) Deterministic() bool {
	d, ok := n.inner.(deterministic)
	return ok && d.Deterministic()
}

// Aggregate computes the mean of each unique score name across results.
// Errored scores are excluded rather than counted as zero.
func Aggregate(results []ObservationResult) map[string]float64 {
	agg, _ := AggregateCounts(results)
	return agg
}

// AggregateCounts returns the same means as [Aggregate] plus, per metric, the
// number of successful scores each mean covers. A count lower than the number
// of observations means some scores errored or the scorer declined some
// observations.
func AggregateCounts(results []ObservationResult) (means map[string]float64, counts map[string]int) {
	sums := make(map[string]float64)
	counts = make(map[string]int)

	for _, r := range results {
		for _, s := range r.Scores {
			if s.Name == "" || s.Error != "" {
				continue
			}
			sums[s.Name] += s.Value
			counts[s.Name]++
		}
	}

	means = make(map[string]float64, len(sums))
	for name, sum := range sums {
		means[name] = sum / float64(counts[name])
	}
	return means, counts
}
