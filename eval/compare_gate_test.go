package eval

import (
	"fmt"
	"testing"
)

// gateSuite builds a suite of n cases where the first fail cases score 0
// on metric "m" and the rest 1, plus a latency_ms score per case.
func gateSuite(name string, n, fail int, latency float64) *SuiteResult {
	s := &SuiteResult{Name: name}
	for i := range n {
		v := 1.0
		if i < fail {
			v = 0
		}
		s.Results = append(s.Results, ObservationResult{
			Observation: Observation{ID: fmt.Sprintf("q%02d", i)},
			Scores:      []Score{{Name: "m", Value: v}, {Name: "latency_ms", Value: latency + float64(i)}},
		})
	}
	s.Aggregate = Aggregate(s.Results)
	return s
}

func TestComparisonGate(t *testing.T) {
	gate := Assertion{Metric: "m", Op: GTE, Threshold: 1}
	gated := func(base, exp *SuiteResult) *Comparison {
		base.Gate(gate)
		exp.Gate(gate)
		return CompareSuites(base, exp)
	}
	tests := []struct {
		name       string
		cmp        *Comparison
		policy     RegressionPolicy
		want       Outcome
		wantMetric map[string]MetricStatus
	}{
		{
			name:       "significant pass-rate drop",
			cmp:        gated(gateSuite("b", 20, 0, 100), gateSuite("e", 20, 6, 100)),
			policy:     RegressionPolicy{Significant: true},
			want:       OutcomeFailed,
			wantMetric: map[string]MetricStatus{"m": MetricRegressed},
		},
		{
			name:       "one flipped case is not significant",
			cmp:        gated(gateSuite("b", 20, 0, 100), gateSuite("e", 20, 1, 100)),
			policy:     RegressionPolicy{Significant: true},
			want:       OutcomePassed,
			wantMetric: map[string]MetricStatus{"m": MetricPassed},
		},
		{
			name:       "point estimate counts any drop",
			cmp:        gated(gateSuite("b", 20, 0, 100), gateSuite("e", 20, 1, 100)),
			policy:     RegressionPolicy{},
			want:       OutcomeFailed,
			wantMetric: map[string]MetricStatus{"m": MetricRegressed},
		},
		{
			name:       "drop within the tolerance",
			cmp:        gated(gateSuite("b", 20, 0, 100), gateSuite("e", 20, 5, 100)),
			policy:     RegressionPolicy{MaxRegression: 0.3},
			want:       OutcomePassed,
			wantMetric: map[string]MetricStatus{"m": MetricPassed},
		},
		{
			name:       "per-metric threshold overrides the default",
			cmp:        gated(gateSuite("b", 20, 0, 100), gateSuite("e", 20, 5, 100)),
			policy:     RegressionPolicy{MaxRegression: 0.3, Thresholds: map[string]float64{"m": 0.1}},
			want:       OutcomeFailed,
			wantMetric: map[string]MetricStatus{"m": MetricRegressed},
		},
		{
			name:       "too few paired cases",
			cmp:        gated(gateSuite("b", 2, 0, 100), gateSuite("e", 2, 2, 100)),
			policy:     RegressionPolicy{MinCases: 3},
			want:       OutcomeInconclusive,
			wantMetric: map[string]MetricStatus{"m": MetricInconclusive},
		},
		{
			name:       "lower-is-better mean rose",
			cmp:        CompareSuites(gateSuite("b", 10, 0, 100), gateSuite("e", 10, 0, 300)),
			policy:     RegressionPolicy{Metrics: []string{"latency_ms"}, Significant: true, LowerIsBetter: map[string]bool{"latency_ms": true}},
			want:       OutcomeFailed,
			wantMetric: map[string]MetricStatus{"latency_ms": MetricRegressed},
		},
		{
			name:       "lower-is-better mean fell",
			cmp:        CompareSuites(gateSuite("b", 10, 0, 300), gateSuite("e", 10, 0, 100)),
			policy:     RegressionPolicy{Metrics: []string{"latency_ms"}, LowerIsBetter: map[string]bool{"latency_ms": true}},
			want:       OutcomePassed,
			wantMetric: map[string]MetricStatus{"latency_ms": MetricPassed},
		},
		{
			name:       "ungated suites check every metric",
			cmp:        CompareSuites(gateSuite("b", 10, 0, 100), gateSuite("e", 10, 0, 100)),
			policy:     RegressionPolicy{},
			want:       OutcomePassed,
			wantMetric: map[string]MetricStatus{"latency_ms": MetricPassed, "m": MetricPassed},
		},
		{
			name:       "metric the candidate never measured",
			cmp:        CompareSuites(gateSuite("b", 10, 0, 100), unmeasuredSuite(10)),
			policy:     RegressionPolicy{Metrics: []string{"m"}},
			want:       OutcomeInconclusive,
			wantMetric: map[string]MetricStatus{"m": MetricInconclusive},
		},
		{
			name:       "a regression wins over an inconclusive metric",
			cmp:        gated(gateSuite("b", 20, 0, 100), gateSuite("e", 20, 8, 100)),
			policy:     RegressionPolicy{Metrics: []string{"m", "missing"}, Significant: true},
			want:       OutcomeFailed,
			wantMetric: map[string]MetricStatus{"m": MetricRegressed, "missing": MetricInconclusive},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := tt.cmp.Gate(tt.policy)
			if rep.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s: %+v", rep.Outcome, tt.want, rep.Metrics)
			}
			if len(rep.Metrics) != len(tt.wantMetric) {
				t.Fatalf("metrics = %+v, want %v", rep.Metrics, tt.wantMetric)
			}
			for _, v := range rep.Metrics {
				if v.Status != tt.wantMetric[v.Metric] {
					t.Fatalf("metric %s = %s (%s), want %s", v.Metric, v.Status, v.Reason, tt.wantMetric[v.Metric])
				}
			}
		})
	}
}

// unmeasuredSuite is a candidate whose subject failed on infrastructure on
// every case.
func unmeasuredSuite(n int) *SuiteResult {
	s := &SuiteResult{Name: "e"}
	for i := range n {
		o := Observation{ID: fmt.Sprintf("q%02d", i)}
		MarkSubjectError(&o, Infra(fmt.Errorf("down")))
		s.Results = append(s.Results, ObservationResult{Observation: o})
	}
	s.SubjectErrors, s.Inconclusive = n, n
	return s
}

func TestComparisonGateReportsCases(t *testing.T) {
	gate := Assertion{Metric: "m", Op: GTE, Threshold: 1}
	base, exp := gateSuite("b", 4, 0, 100), gateSuite("e", 4, 1, 100)
	exp.Results[3].Scores[0] = Score{Name: "m", Error: "429", Inconclusive: true}
	exp.Inconclusive = 1
	base.Gate(gate)
	exp.GateWith(GatePolicy{MaxInconclusive: 1}, gate)
	rep := CompareSuites(base, exp).Gate(RegressionPolicy{})
	if len(rep.Regressions) != 1 || rep.Regressions[0].ID != "q00" {
		t.Fatalf("regressions = %+v", rep.Regressions)
	}
	if len(rep.Inconclusive) != 1 || rep.Inconclusive[0].ID != "q03" || rep.ExpInconclusive != 1 {
		t.Fatalf("inconclusive = %+v (%d)", rep.Inconclusive, rep.ExpInconclusive)
	}
	if rep.Metrics[0].N != 3 || rep.Metrics[0].McNemarP == nil {
		t.Fatalf("pass-rate verdict = %+v, want 3 paired cases with a McNemar p", rep.Metrics[0])
	}
	if rep.Confidence != DefaultConfidence {
		t.Fatalf("confidence = %v", rep.Confidence)
	}
}

func TestRegressionPolicyValidate(t *testing.T) {
	for _, p := range []RegressionPolicy{
		{MaxRegression: -1},
		{Thresholds: map[string]float64{"m": -0.1}},
		{MinCases: -1},
	} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	if err := (RegressionPolicy{MaxRegression: 0.1, MinCases: 3}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestComparisonGateDefaultsToGatedMetrics(t *testing.T) {
	gate := Assertion{Metric: "m", Op: GTE, Threshold: 1}
	base := gateSuite("b", 10, 0, 100)
	base.Gate(gate)
	// The candidate lost every case, so only the baseline has verdicts.
	rep := CompareSuites(base, unmeasuredSuite(10)).Gate(RegressionPolicy{})
	if len(rep.Metrics) != 1 || rep.Metrics[0].Metric != "m" || rep.Outcome != OutcomeInconclusive {
		t.Fatalf("report = %+v, want only the gated metric, inconclusive", rep)
	}
}
