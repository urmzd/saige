package eval

import (
	"fmt"
	"slices"
)

// RegressionPolicy decides when a [Comparison] of a candidate (exp) against
// a baseline (base) is a regression; see [Comparison.Gate].
type RegressionPolicy struct {
	// Metrics are the metrics to check. Empty checks every metric either
	// arm gated (with a [Score.Passed] verdict) or, when no metric was
	// gated, every metric scored on both arms plus those the candidate
	// lost.
	Metrics []string
	// MaxRegression is the largest drop a metric may show and still pass,
	// in the metric's units: a share of cases for a gated metric, the
	// metric's own scale otherwise. Zero tolerates no drop.
	MaxRegression float64
	// Thresholds overrides MaxRegression for individual metrics.
	Thresholds map[string]float64
	// Significant counts a drop as a regression only when the
	// comparison's interval for it (see [Comparison.Confidence]) lies
	// entirely beyond the tolerated drop. Without it the point estimate
	// decides, so noise can fail the gate.
	Significant bool
	// MinCases is the fewest paired cases a metric needs to be decided.
	// With fewer it is inconclusive. Zero means 1.
	MinCases int
	// LowerIsBetter names ungated metrics, such as latencies, where a rise
	// is the regression. Gated metrics compare pass rates, where higher
	// is always better.
	LowerIsBetter map[string]bool
}

// threshold returns the tolerated drop for metric.
func (p RegressionPolicy) threshold(metric string) float64 {
	if t, ok := p.Thresholds[metric]; ok {
		return t
	}
	return p.MaxRegression
}

// Validate rejects negative thresholds and a negative MinCases.
func (p RegressionPolicy) Validate() error {
	if p.MaxRegression < 0 {
		return fmt.Errorf("max regression %g must not be negative", p.MaxRegression)
	}
	for m, t := range p.Thresholds {
		if t < 0 {
			return fmt.Errorf("threshold for %s %g must not be negative", m, t)
		}
	}
	if p.MinCases < 0 {
		return fmt.Errorf("min cases %d must not be negative", p.MinCases)
	}
	return nil
}

// MetricBasis is what a [MetricVerdict] compares.
type MetricBasis string

const (
	// BasisPassRate compares the share of cases that passed their gate,
	// paired by case with McNemar's interval (or case-level bootstrap
	// for sampled cases).
	BasisPassRate MetricBasis = "pass_rate"
	// BasisMean compares the metric's mean, paired by case with a
	// bootstrap interval.
	BasisMean MetricBasis = "mean"
)

// MetricStatus is the verdict on one metric of a regression gate.
type MetricStatus string

// Statuses of a [MetricVerdict].
const (
	MetricPassed       MetricStatus = "passed"
	MetricRegressed    MetricStatus = "regressed"
	MetricInconclusive MetricStatus = "inconclusive"
)

// MetricVerdict is one metric's result in a [RegressionReport]. Base and Exp
// are each arm's pass rate or mean; Delta is the paired difference exp minus
// base over N cases with its interval [Low, High]. For a lower-is-better
// metric a positive Delta is the regression.
type MetricVerdict struct {
	Metric        string       `json:"metric"`
	Basis         MetricBasis  `json:"basis,omitempty"`
	Base          float64      `json:"base"`
	Exp           float64      `json:"exp"`
	Delta         float64      `json:"delta"`
	Low           float64      `json:"low"`
	High          float64      `json:"high"`
	N             int          `json:"n"`
	McNemarP      *float64     `json:"mcnemar_p,omitempty"`
	LowerIsBetter bool         `json:"lower_is_better,omitempty"`
	MaxRegression float64      `json:"max_regression"`
	Status        MetricStatus `json:"status"`
	Reason        string       `json:"reason,omitempty"`
}

// RegressionReport is the result of [Comparison.Gate].
type RegressionReport struct {
	Name string `json:"name,omitempty"`
	Base string `json:"base,omitempty"`
	Exp  string `json:"exp,omitempty"`
	// Outcome is [OutcomeFailed] when a metric regressed,
	// [OutcomeInconclusive] when none did but a metric could not be
	// decided or none was checked, and [OutcomePassed] otherwise.
	Outcome    Outcome         `json:"outcome"`
	Confidence float64         `json:"confidence,omitempty"`
	Metrics    []MetricVerdict `json:"metrics"`
	// Regressions lists the cases that got worse in the checked metrics.
	// They explain a regression; the metric verdicts decide it.
	Regressions []CaseDiff `json:"regressions,omitempty"`
	// Inconclusive lists the cases of the checked metrics that one arm
	// could not measure.
	Inconclusive     []CaseDiff `json:"inconclusive,omitempty"`
	BaseInconclusive int        `json:"base_inconclusive,omitempty"`
	ExpInconclusive  int        `json:"exp_inconclusive,omitempty"`
	Warnings         []string   `json:"warnings,omitempty"`
}

// Gate decides whether the candidate regressed against the baseline under
// policy. For each metric it uses the paired pass-rate comparison when both
// arms were gated on it and the paired mean difference otherwise; a metric
// regresses when its drop exceeds the tolerated one (significantly, with
// policy.Significant). A metric with fewer than policy.MinCases paired
// cases, or scored on one arm only, is inconclusive: cases one arm could not
// measure because infrastructure failed are never paired, so an outage
// lowers the count instead of reading as a drop.
func (c *Comparison) Gate(policy RegressionPolicy) RegressionReport {
	rep := RegressionReport{
		Name: c.Name, Base: c.Base, Exp: c.Exp,
		Confidence:       c.Confidence,
		BaseInconclusive: c.BaseInconclusive,
		ExpInconclusive:  c.ExpInconclusive,
		Warnings:         slices.Clone(c.Warnings),
	}
	minCases := max(policy.MinCases, 1)
	metrics := policy.Metrics
	if len(metrics) == 0 {
		metrics = c.defaultGateMetrics()
	}
	metrics = slices.Compact(slices.Sorted(slices.Values(metrics)))

	checked := map[string]bool{}
	for _, m := range metrics {
		checked[m] = true
		v := c.metricVerdict(m, policy, minCases)
		rep.Metrics = append(rep.Metrics, v)
	}
	for _, d := range c.Cases {
		if !checked[d.Metric] {
			continue
		}
		switch d.Status {
		case CaseRegressed:
			rep.Regressions = append(rep.Regressions, d)
		case CaseInconclusive:
			rep.Inconclusive = append(rep.Inconclusive, d)
		}
	}

	rep.Outcome = OutcomePassed
	if len(rep.Metrics) == 0 {
		rep.Outcome = OutcomeInconclusive
	}
	for _, v := range rep.Metrics {
		switch v.Status {
		case MetricRegressed:
			rep.Outcome = OutcomeFailed
		case MetricInconclusive:
			if rep.Outcome != OutcomeFailed {
				rep.Outcome = OutcomeInconclusive
			}
		}
	}
	return rep
}

// defaultGateMetrics returns the metrics either arm gated (those with a
// [Score.Passed] verdict) or, when neither gated any, those scored on both
// arms plus those only the baseline scored.
func (c *Comparison) defaultGateMetrics() []string {
	gated := map[string]bool{}
	for _, results := range [][]ObservationResult{c.BaseResults, c.ExpResults} {
		for _, r := range results {
			for _, s := range r.Scores {
				if s.Passed != nil {
					gated[s.Name] = true
				}
			}
		}
	}
	var out []string
	for m := range c.PassStats {
		gated[m] = true
	}
	for m := range gated {
		out = append(out, m)
	}
	if len(out) > 0 {
		return out
	}
	for m := range c.DeltaStats {
		out = append(out, m)
	}
	return append(out, c.MissingInExp...)
}

func (c *Comparison) metricVerdict(metric string, policy RegressionPolicy, minCases int) MetricVerdict {
	v := MetricVerdict{Metric: metric, MaxRegression: policy.threshold(metric)}
	if ps, ok := c.PassStats[metric]; ok {
		v.Basis = BasisPassRate
		v.Base, v.Exp = ps.Base.Value, ps.Exp.Value
		v.Delta, v.Low, v.High, v.N = ps.Diff.Value, ps.Diff.Low, ps.Diff.High, ps.Diff.N
		if !ps.CaseRates {
			p := ps.McNemarP
			v.McNemarP = &p
		}
	} else if ds, ok := c.DeltaStats[metric]; ok {
		v.Basis = BasisMean
		v.Base, v.Exp = c.BaseAggregate[metric], c.ExpAggregate[metric]
		v.Delta, v.Low, v.High, v.N = ds.Delta, ds.CILow, ds.CIHigh, ds.N
		v.LowerIsBetter = policy.LowerIsBetter[metric]
	} else {
		v.Status = MetricInconclusive
		switch {
		case slices.Contains(c.MissingInExp, metric):
			v.Reason = "the candidate has no successful score for it"
		case slices.Contains(c.MissingInBase, metric):
			v.Reason = "the baseline has no successful score for it"
		default:
			v.Reason = "no case scored it on both arms"
		}
		return v
	}
	if v.N < minCases {
		v.Status = MetricInconclusive
		v.Reason = fmt.Sprintf("%d paired cases, fewer than the %d required", v.N, minCases)
		return v
	}

	// gain is the improvement, positive when exp is better.
	gain, gainHigh := v.Delta, v.High
	if v.LowerIsBetter {
		gain, gainHigh = -v.Delta, -v.Low
	}
	v.Status = MetricPassed
	switch {
	case policy.Significant && gainHigh < -v.MaxRegression-eqTolerance:
		v.Status = MetricRegressed
		v.Reason = fmt.Sprintf("dropped by %.3g, interval entirely beyond the %.3g tolerated", -gain, v.MaxRegression)
	case !policy.Significant && gain < -v.MaxRegression-eqTolerance:
		v.Status = MetricRegressed
		v.Reason = fmt.Sprintf("dropped by %.3g, more than the %.3g tolerated", -gain, v.MaxRegression)
	case gain < -v.MaxRegression-eqTolerance:
		v.Reason = fmt.Sprintf("dropped by %.3g, not significant", -gain)
	}
	return v
}
