package eval

import (
	"cmp"
	"math"
	"slices"

	"github.com/urmzd/saige/eval/analysis"
)

// Group is one cell of a grouped view: the label values that define it and
// the statistic computed over its results.
type Group struct {
	Labels Labels `json:"labels"`
	Stat   Stat   `json:"stat"`
}

// GroupBy groups results by the values of the named labels and returns the
// mean of metric in each group with a normal-approximation interval at
// [DefaultConfidence]. A result without one of the labels groups under the
// empty value. Errored scores are left out of the mean, and
// [analysis.Stat.Completeness] reports the share of the group's
// observations that produced a value, so a mean over few cases is visible
// as such. With no labels, all results form one group. Groups are sorted by
// their labels.
func GroupBy(results []ObservationResult, metric string, by ...string) []Group {
	return group(results, by, func(rs []ObservationResult) Stat {
		var values []float64
		for _, r := range rs {
			for _, s := range r.Scores {
				if s.Name == metric && s.Error == "" {
					values = append(values, s.Value)
				}
			}
		}
		return analysis.MeanStat(values, len(rs), DefaultConfidence)
	})
}

// GroupPassRate groups results like [GroupBy] and returns the share of
// metric's gate verdicts ([Score.Passed]) that passed in each group, with a
// Wilson interval at [DefaultConfidence]. Scores without a verdict are not
// graded and lower Completeness instead of the rate. Gate the suite first
// with [SuiteResult.Gate] or [WithAssertions].
func GroupPassRate(results []ObservationResult, metric string, by ...string) []Group {
	return group(results, by, func(rs []ObservationResult) Stat {
		graded, passed := 0, 0
		for _, r := range rs {
			for _, s := range r.Scores {
				if s.Name != metric || s.Passed == nil {
					continue
				}
				graded++
				if *s.Passed {
					passed++
				}
			}
		}
		return analysis.Rate(passed, graded, len(rs), DefaultConfidence)
	})
}

func group(results []ObservationResult, by []string, stat func([]ObservationResult) Stat) []Group {
	type bucket struct {
		labels  Labels
		results []ObservationResult
	}
	buckets := map[string]*bucket{}
	for _, r := range results {
		labels := make(Labels, len(by))
		for _, k := range by {
			labels[k] = r.Observation.Labels[k]
		}
		key := labels.String()
		b, ok := buckets[key]
		if !ok {
			b = &bucket{labels: labels}
			buckets[key] = b
		}
		b.results = append(b.results, r)
	}
	out := make([]Group, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, Group{Labels: b.labels, Stat: stat(b.results)})
	}
	slices.SortFunc(out, func(a, b Group) int { return cmp.Compare(a.Labels.String(), b.Labels.String()) })
	return out
}

// QuestionRate is the pass rate of one question, one case asked several
// times, under one variant.
type QuestionRate struct {
	ID      string `json:"id"`
	Turn    int    `json:"turn,omitempty"`
	Variant string `json:"variant,omitempty"`
	// Rate is the share of graded samples that passed, with a Wilson
	// interval; Rate.N is the number of graded samples.
	Rate   Stat `json:"rate"`
	Passed int  `json:"passed"`
	// PassAtK maps each requested k to the unbiased estimate of the chance
	// that at least one of k samples passes. A k above the number of graded
	// samples is left out, since the estimate is undefined there.
	PassAtK map[int]float64 `json:"pass_at_k,omitempty"`
}

// QuestionPassRates reduces sampled results to one [QuestionRate] per
// question: results with the same ID, Turn, and [LabelVariant] are the
// samples of one question, as produced by [WithRepeats] or
// [Sampler.Replicate]. Verdicts come from [Score.Passed], so gate the suite
// first. ks lists the k values for pass@k. Rates are sorted by variant, ID,
// and Turn; questions with no graded sample are left out.
func QuestionPassRates(results []ObservationResult, metric string, ks ...int) []QuestionRate {
	type qkey struct {
		variant, id string
		turn        int
	}
	type tally struct{ graded, passed, samples int }
	tallies := map[qkey]*tally{}
	for _, r := range results {
		k := qkey{r.Observation.Labels[LabelVariant], r.Observation.ID, r.Observation.Turn}
		t, ok := tallies[k]
		if !ok {
			t = &tally{}
			tallies[k] = t
		}
		t.samples++
		for _, s := range r.Scores {
			if s.Name != metric || s.Passed == nil {
				continue
			}
			t.graded++
			if *s.Passed {
				t.passed++
			}
		}
	}

	out := make([]QuestionRate, 0, len(tallies))
	for k, t := range tallies {
		if t.graded == 0 {
			continue
		}
		q := QuestionRate{
			ID:      k.id,
			Turn:    k.turn,
			Variant: k.variant,
			Rate:    analysis.Rate(t.passed, t.graded, t.samples, DefaultConfidence),
			Passed:  t.passed,
		}
		for _, kk := range ks {
			if v := analysis.PassAtK(t.graded, t.passed, kk); !math.IsNaN(v) {
				if q.PassAtK == nil {
					q.PassAtK = map[int]float64{}
				}
				q.PassAtK[kk] = v
			}
		}
		out = append(out, q)
	}
	slices.SortFunc(out, func(a, b QuestionRate) int {
		return cmp.Or(cmp.Compare(a.Variant, b.Variant), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Turn, b.Turn))
	})
	return out
}

// PassAtK averages the per-question pass@k estimate of [QuestionPassRates]
// over the questions that have at least k graded samples, separately per
// variant (keyed by its [LabelVariant] value, "" for unlabeled results).
func PassAtK(results []ObservationResult, metric string, k int) map[string]Stat {
	values := map[string][]float64{}
	questions := map[string]int{}
	for _, q := range QuestionPassRates(results, metric, k) {
		questions[q.Variant]++
		if v, ok := q.PassAtK[k]; ok {
			values[q.Variant] = append(values[q.Variant], v)
		}
	}
	out := make(map[string]Stat, len(questions))
	for variant, n := range questions {
		out[variant] = analysis.MeanStat(values[variant], n, DefaultConfidence)
	}
	return out
}
