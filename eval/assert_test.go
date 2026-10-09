package eval

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func assertionSuite() *SuiteResult {
	results := []ObservationResult{
		{Observation: Observation{ID: "good"}, Scores: []Score{{Name: "token_f1", Value: 0.9}}},
		{Observation: Observation{ID: "bad"}, Scores: []Score{{Name: "token_f1", Value: 0.2}}},
	}
	return &SuiteResult{Results: results, Aggregate: Aggregate(results)}
}

func TestSuiteCheck(t *testing.T) {
	tests := []struct {
		name       string
		suite      func() *SuiteResult
		assertions []Assertion
		wantCases  []string // CaseID of each violation, "" for aggregate or suite level
		wantReason bool     // whether the first violation carries a Reason
	}{
		{
			name:       "every case flags the failing case",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "token_f1", Op: GTE, Threshold: 0.5, Scope: EveryCase}},
			wantCases:  []string{"bad"},
		},
		{
			name:       "empty scope means every case",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "token_f1", Op: LTE, Threshold: 0.5}},
			wantCases:  []string{"good"},
		},
		{
			name:       "aggregate passes",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "token_f1", Op: GTE, Threshold: 0.5, Scope: OnAggregate}},
		},
		{
			name:       "aggregate fails",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "token_f1", Op: GTE, Threshold: 0.6, Scope: OnAggregate}},
			wantCases:  []string{""},
		},
		{
			name:       "eq holds within float noise",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "token_f1", Op: EQ, Threshold: 0.55, Scope: OnAggregate}},
		},
		{
			name:       "missing metric is a violation",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "tokn_f1", Op: GTE, Threshold: 0.5}},
			wantCases:  []string{""},
			wantReason: true,
		},
		{
			name: "errored score is a violation",
			suite: func() *SuiteResult {
				return &SuiteResult{Results: []ObservationResult{
					{Observation: Observation{ID: "e"}, Scores: []Score{{Name: "judge", Error: "boom"}}},
				}}
			},
			assertions: []Assertion{{Metric: "judge", Op: GTE, Threshold: 0}},
			wantCases:  []string{"e"},
			wantReason: true,
		},
		{
			name: "incomplete suite fails",
			suite: func() *SuiteResult {
				s := assertionSuite()
				s.Incomplete = 3
				return s
			},
			wantCases:  []string{""},
			wantReason: true,
		},
		{
			name:       "unknown op",
			suite:      assertionSuite,
			assertions: []Assertion{{Metric: "token_f1", Op: ">", Threshold: 0.5}},
			wantCases:  []string{""},
			wantReason: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.suite().Check(tt.assertions...)
			if len(got) != len(tt.wantCases) {
				t.Fatalf("violations: got %+v, want cases %v", got, tt.wantCases)
			}
			for i, v := range got {
				if v.CaseID != tt.wantCases[i] {
					t.Errorf("violation %d: CaseID %q, want %q", i, v.CaseID, tt.wantCases[i])
				}
			}
			if len(got) > 0 && tt.wantReason != (got[0].Reason != "") {
				t.Errorf("violation reason %q, want reason present=%v", got[0].Reason, tt.wantReason)
			}
		})
	}
}

func TestSuiteCheckViolationValues(t *testing.T) {
	got := assertionSuite().Check(Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.5})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	v := got[0]
	if v.Metric != "token_f1" || v.Value != 0.2 || v.Threshold != 0.5 {
		t.Errorf("unexpected violation %+v", v)
	}
	if v.String() == "" {
		t.Error("String should describe the violation")
	}
}

func TestPassRate(t *testing.T) {
	results := assertionSuite().Results
	tests := []struct {
		name   string
		res    []ObservationResult
		metric string
		op     Op
		thr    float64
		want   float64
	}{
		{name: "half pass", res: results, metric: "token_f1", op: GTE, thr: 0.5, want: 0.5},
		{name: "all pass", res: results, metric: "token_f1", op: LTE, thr: 1, want: 1},
		{name: "missing metric", res: results, metric: "nope", op: GTE, thr: 0, want: 0},
		{
			name: "errored score fails",
			res: append(append([]ObservationResult(nil), results...),
				ObservationResult{Scores: []Score{{Name: "token_f1", Error: "x"}}}),
			metric: "token_f1", op: GTE, thr: 0, want: 2.0 / 3.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertClose(t, "pass rate", PassRate(tt.res, tt.metric, tt.op, tt.thr), tt.want, 1e-9)
		})
	}
}

// labeledSuite has two variants with token_f1 values: a scores 0.9 and 0.8,
// b scores 0.2 and 0.9.
func labeledSuite() *SuiteResult {
	row := func(id, variant string, v float64) ObservationResult {
		return ObservationResult{
			Observation: Observation{ID: id, Labels: Labels{LabelVariant: variant}},
			Scores:      []Score{{Name: "token_f1", Value: v}},
		}
	}
	results := []ObservationResult{row("q1", "a", 0.9), row("q2", "a", 0.8), row("q1", "b", 0.2), row("q2", "b", 0.9)}
	return &SuiteResult{Results: results, Aggregate: Aggregate(results)}
}

func TestSuiteCheckWhereAndMinPassRate(t *testing.T) {
	tests := []struct {
		name       string
		assertion  Assertion
		wantCount  int
		wantReason string // substring of the last violation's reason
	}{
		{
			name:      "where limits every case to one variant",
			assertion: Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.5, Where: Where{LabelVariant: "a"}},
		},
		{
			name:      "where on failing variant",
			assertion: Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.5, Where: Where{LabelVariant: "b"}},
			wantCount: 1,
		},
		{
			name:      "aggregate under where uses the scoped mean",
			assertion: Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.8, Scope: OnAggregate, Where: Where{LabelVariant: "a"}},
		},
		{
			name:      "aggregate under where fails on the scoped mean",
			assertion: Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.6, Scope: OnAggregate, Where: Where{LabelVariant: "b"}},
			wantCount: 1,
		},
		{
			name:       "where matching nothing is a violation",
			assertion:  Assertion{Metric: "token_f1", Op: GTE, Threshold: 0, Where: Where{LabelVariant: "c"}},
			wantCount:  1,
			wantReason: "variant=c",
		},
		{
			name:      "min pass rate met",
			assertion: Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.5, MinPassRate: 0.75},
		},
		{
			name:       "min pass rate missed reports cases and a summary",
			assertion:  Assertion{Metric: "token_f1", Op: GTE, Threshold: 0.85, MinPassRate: 0.75},
			wantCount:  3,
			wantReason: "pass rate 0.5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := labeledSuite().Check(tt.assertion)
			if len(got) != tt.wantCount {
				t.Fatalf("violations = %+v, want %d", got, tt.wantCount)
			}
			if tt.wantReason != "" && !strings.Contains(got[len(got)-1].Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", got[len(got)-1].Reason, tt.wantReason)
			}
			for _, v := range got {
				if len(tt.assertion.Where) > 0 && v.Where.String() != tt.assertion.Where.String() {
					t.Errorf("violation Where = %v, want %v", v.Where, tt.assertion.Where)
				}
			}
		})
	}
}

func TestSuiteGate(t *testing.T) {
	tests := []struct {
		name        string
		assertions  []Assertion
		wantOutcome Outcome
		wantPassed  []*bool // per result, the Passed of its token_f1 score
	}{
		{
			name:        "no assertions is unasserted",
			wantOutcome: OutcomeUnasserted,
			wantPassed:  []*bool{nil, nil, nil, nil},
		},
		{
			name:        "every case stamps each score",
			assertions:  []Assertion{{Metric: "token_f1", Op: GTE, Threshold: 0.5}},
			wantOutcome: OutcomeFailed,
			wantPassed:  []*bool{ptr(true), ptr(true), ptr(false), ptr(true)},
		},
		{
			name:        "scoped assertion stamps only its scope",
			assertions:  []Assertion{{Metric: "token_f1", Op: GTE, Threshold: 0.5, Where: Where{LabelVariant: "a"}}},
			wantOutcome: OutcomePassed,
			wantPassed:  []*bool{ptr(true), ptr(true), nil, nil},
		},
		{
			name: "two assertions on one metric must both hold",
			assertions: []Assertion{
				{Metric: "token_f1", Op: GTE, Threshold: 0.5},
				{Metric: "token_f1", Op: LTE, Threshold: 0.85},
			},
			wantOutcome: OutcomeFailed,
			wantPassed:  []*bool{ptr(false), ptr(true), ptr(false), ptr(false)},
		},
		{
			name:        "aggregate assertion stamps nothing",
			assertions:  []Assertion{{Metric: "token_f1", Op: GTE, Threshold: 0.5, Scope: OnAggregate}},
			wantOutcome: OutcomePassed,
			wantPassed:  []*bool{nil, nil, nil, nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := labeledSuite()
			// A stale stamp from an earlier gate must be replaced.
			s.Results[0].Scores[0].Passed = ptr(false)
			if got := s.Gate(tt.assertions...); got != tt.wantOutcome || s.Outcome != got {
				t.Errorf("outcome = %q (suite %q), want %q", got, s.Outcome, tt.wantOutcome)
			}
			if (s.Outcome == OutcomeFailed) != (len(s.Violations) > 0) {
				t.Errorf("violations %+v do not match outcome %q", s.Violations, s.Outcome)
			}
			for i, want := range tt.wantPassed {
				got := s.Results[i].Scores[0].Passed
				if (got == nil) != (want == nil) || (got != nil && *got != *want) {
					t.Errorf("result %d Passed = %v, want %v", i, fmtBoolPtr(got), fmtBoolPtr(want))
				}
			}
		})
	}
}

func fmtBoolPtr(b *bool) string {
	if b == nil {
		return "nil"
	}
	return strconv.FormatBool(*b)
}

func TestRunWithAssertions(t *testing.T) {
	obs := []Observation{{ID: "a", Output: json.RawMessage(`1`)}, {ID: "b", Output: json.RawMessage(`0`)}}
	scorer := NewScorerFunc("v", func(_ context.Context, o Observation) (Score, error) {
		var v float64
		_ = json.Unmarshal(o.Output, &v)
		return Score{Name: "v", Value: v}, nil
	})
	suite, err := Run(context.Background(), "gated", obs, []Scorer{scorer},
		WithAssertions(Assertion{Metric: "v", Op: GTE, Threshold: 1}))
	if err != nil {
		t.Fatalf("a failed gate is not a run error, got %v", err)
	}
	if suite.Outcome != OutcomeFailed || len(suite.Violations) != 1 || suite.Violations[0].CaseID != "b" {
		t.Errorf("outcome %q violations %+v, want failed on b", suite.Outcome, suite.Violations)
	}

	ungated, err := Run(context.Background(), "plain", obs, []Scorer{scorer})
	if err != nil {
		t.Fatal(err)
	}
	if ungated.Outcome != "" {
		t.Errorf("ungated outcome = %q, want empty", ungated.Outcome)
	}
}
