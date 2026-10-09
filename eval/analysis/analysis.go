// Package analysis holds the statistics behind eval reports: rate and mean
// intervals, completeness, pass@k, and paired comparisons. Every function is
// pure and works on plain counts and values, so it has no dependency on the
// eval types and can be reused over results loaded from any store.
package analysis

import (
	"math"
	"math/rand/v2"
	"sort"
)

// DefaultConfidence is the confidence level used when a caller passes a
// level outside (0, 1).
const DefaultConfidence = 0.95

// Stat is one estimate with its interval.
//
// N is the number of values or trials behind Value, and [Low, High] is the
// interval at the requested confidence. Completeness is N divided by the
// number of values that were expected, reported next to the estimate and
// never folded into it: a 0.9 pass rate over 3 of 30 expected cases is a
// 0.9 with Completeness 0.1, not a 0.09.
type Stat struct {
	N            int     `json:"n"`
	Value        float64 `json:"value"`
	Low          float64 `json:"low"`
	High         float64 `json:"high"`
	Completeness float64 `json:"completeness"`
}

// Contains reports whether x lies inside the interval.
func (s Stat) Contains(x float64) bool { return s.Low <= x && x <= s.High }

// Z returns the two-sided standard normal quantile for a confidence level,
// such as 1.96 for 0.95. A level outside (0, 1) means [DefaultConfidence].
func Z(confidence float64) float64 {
	if confidence <= 0 || confidence >= 1 {
		confidence = DefaultConfidence
	}
	return math.Sqrt2 * math.Erfinv(confidence)
}

// Completeness returns graded / expected, or 0 when nothing was expected.
func Completeness(graded, expected int) float64 {
	if expected <= 0 {
		return 0
	}
	return float64(graded) / float64(expected)
}

// Wilson returns the Wilson score interval for successes out of n trials.
// Unlike the normal approximation it stays inside [0, 1] and is usable for
// small n and for rates near 0 or 1. With n == 0 the interval is [0, 1].
func Wilson(successes, n int, confidence float64) (low, high float64) {
	if n <= 0 {
		return 0, 1
	}
	successes = min(max(successes, 0), n)
	z := Z(confidence)
	nf := float64(n)
	p := float64(successes) / nf
	z2 := z * z
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	half := z * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	return clamp(center-half, 0, 1), clamp(center+half, 0, 1)
}

// Rate returns successes / n with its Wilson interval. expected is the
// number of trials that should have been graded; pass n when every trial
// was.
func Rate(successes, n, expected int, confidence float64) Stat {
	low, high := Wilson(successes, n, confidence)
	s := Stat{N: n, Low: low, High: high, Completeness: Completeness(n, expected)}
	if n > 0 {
		s.Value = float64(successes) / float64(n)
	}
	return s
}

// MeanStat returns the mean of values with a normal-approximation interval
// (mean plus or minus z times the standard error). With fewer than two
// values the interval collapses to the mean. expected is the number of
// values that should have been recorded; pass len(values) when all were.
func MeanStat(values []float64, expected int, confidence float64) Stat {
	n := len(values)
	s := Stat{N: n, Completeness: Completeness(n, expected)}
	if n == 0 {
		return s
	}
	s.Value = Mean(values)
	s.Low, s.High = s.Value, s.Value
	if n < 2 {
		return s
	}
	half := Z(confidence) * StdDev(values) / math.Sqrt(float64(n))
	s.Low, s.High = s.Value-half, s.Value+half
	return s
}

// Mean returns the arithmetic mean, or 0 for no values.
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

// StdDev returns the sample standard deviation (n-1 denominator), or 0 for
// fewer than two values.
func StdDev(values []float64) float64 {
	n := len(values)
	if n < 2 {
		return 0
	}
	m := Mean(values)
	ss := 0.0
	for _, v := range values {
		ss += (v - m) * (v - m)
	}
	return math.Sqrt(ss / float64(n-1))
}

// Quantile returns the q quantile of values (0 <= q <= 1) by linear
// interpolation between closest ranks. values need not be sorted and is not
// modified. It returns 0 for no values.
func Quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	q = clamp(q, 0, 1)
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// PassAtK returns the unbiased pass@k estimate for one question sampled n
// times with c passing samples: the probability that at least one of k
// samples drawn without replacement passes. It returns NaN when k is not in
// [1, n] or c is not in [0, n], since the estimate is undefined there.
func PassAtK(n, c, k int) float64 {
	if k < 1 || k > n || c < 0 || c > n {
		return math.NaN()
	}
	if n-c < k {
		return 1
	}
	// 1 - C(n-c, k) / C(n, k), as a running product for stability.
	prod := 1.0
	for i := n - c + 1; i <= n; i++ {
		prod *= 1 - float64(k)/float64(i)
	}
	return 1 - prod
}

// Paired counts the outcomes of the same cases graded pass or fail under
// two arms, A and B.
type Paired struct {
	Both    int `json:"both"`
	OnlyA   int `json:"only_a"`
	OnlyB   int `json:"only_b"`
	Neither int `json:"neither"`
}

// N is the number of pairs.
func (p Paired) N() int { return p.Both + p.OnlyA + p.OnlyB + p.Neither }

// RateA is the pass rate of arm A over the pairs.
func (p Paired) RateA() float64 { return ratio(p.Both+p.OnlyA, p.N()) }

// RateB is the pass rate of arm B over the pairs.
func (p Paired) RateB() float64 { return ratio(p.Both+p.OnlyB, p.N()) }

// Diff returns the paired difference in pass rate, B minus A, with the
// Wald interval built on McNemar's variance. Only discordant pairs move the
// estimate, so the interval is much tighter than comparing two independent
// rates when the arms agree on most cases. With no pairs the interval is
// [-1, 1].
func (p Paired) Diff(confidence float64) Stat {
	n := p.N()
	s := Stat{N: n, Low: -1, High: 1, Completeness: Completeness(n, n)}
	if n == 0 {
		return s
	}
	nf := float64(n)
	b, c := float64(p.OnlyA), float64(p.OnlyB)
	s.Value = (c - b) / nf
	variance := (b + c) - (c-b)*(c-b)/nf
	half := Z(confidence) * math.Sqrt(math.Max(variance, 0)) / nf
	s.Low = clamp(s.Value-half, -1, 1)
	s.High = clamp(s.Value+half, -1, 1)
	return s
}

// McNemarP returns the exact two-sided McNemar p-value: the probability,
// under no difference between the arms, of a split of the discordant pairs
// at least as uneven as the observed one. With no discordant pairs it is 1.
func (p Paired) McNemarP() float64 {
	m := p.OnlyA + p.OnlyB
	if m == 0 {
		return 1
	}
	k := min(p.OnlyA, p.OnlyB)
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += math.Exp(logChoose(m, i) - float64(m)*math.Ln2)
	}
	return math.Min(1, 2*tail)
}

// Bootstrap returns a percentile bootstrap interval for the mean of values,
// drawn from resamples resamples with a fixed seed so the same input always
// gives the same interval. With fewer than two values the interval
// collapses to the mean.
func Bootstrap(values []float64, resamples int, confidence float64, seed uint64) (low, high float64) {
	m := Mean(values)
	if len(values) < 2 || resamples < 1 {
		return m, m
	}
	if confidence <= 0 || confidence >= 1 {
		confidence = DefaultConfidence
	}
	rng := rand.New(rand.NewPCG(seed, uint64(len(values)))) //nolint:gosec // reproducible resampling, not security
	means := make([]float64, resamples)
	for r := range means {
		sum := 0.0
		for range values {
			sum += values[rng.IntN(len(values))]
		}
		means[r] = sum / float64(len(values))
	}
	sort.Float64s(means)
	alpha := (1 - confidence) / 2
	return nearestRank(means, alpha), nearestRank(means, 1-alpha)
}

// nearestRank returns the q quantile of sorted values by nearest rank.
func nearestRank(sorted []float64, q float64) float64 {
	idx := int(q * float64(len(sorted)-1))
	idx = min(max(idx, 0), len(sorted)-1)
	return sorted[idx]
}

func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func clamp(x, lo, hi float64) float64 { return math.Min(math.Max(x, lo), hi) }
