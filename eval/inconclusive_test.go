package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestIsInfra(t *testing.T) {
	providerErr := func(kind types.ErrorKind) error {
		return &types.ProviderError{Provider: "p", Model: "m", Kind: kind, Err: errors.New("boom")}
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("wrong answer format"), false},
		{"marked", Infra(errors.New("connector unreachable")), true},
		{"wrapped sentinel", fmt.Errorf("mcp: %w", ErrInfra), true},
		{"canceled", fmt.Errorf("run: %w", context.Canceled), true},
		{"deadline", context.DeadlineExceeded, true},
		{"rate limit", providerErr(types.ErrorKindRateLimit), true},
		{"unavailable", providerErr(types.ErrorKindUnavailable), true},
		{"transient", providerErr(types.ErrorKindTransient), true},
		{"auth", providerErr(types.ErrorKindAuth), true},
		{"invalid request", providerErr(types.ErrorKindInvalidRequest), false},
		{"context length", providerErr(types.ErrorKindContextLength), false},
		{"content filter", providerErr(types.ErrorKindContentFilter), false},
		{"truncated", &types.ResponseTruncatedError{FinishReason: "length"}, false},
		{"fallback ends transient", &types.FallbackError{Errors: []error{providerErr(types.ErrorKindInvalidRequest), providerErr(types.ErrorKindUnavailable)}}, true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"net op error", &net.OpError{Op: "dial", Err: errors.New("no route to host")}, true},
		{"batch request expired", &types.BatchRequestError{Outcome: types.BatchExpiredOutcome}, true},
		{"batch request canceled", fmt.Errorf("judge: %w", &types.BatchRequestError{Outcome: types.BatchCanceledOutcome}), true},
		{"batch request errored without a cause", &types.BatchRequestError{Outcome: types.BatchErrored, Message: "missing from the vendor's results"}, true},
		{"batch request errored on overload", &types.BatchRequestError{Outcome: types.BatchErrored, Err: providerErr(types.ErrorKindUnavailable)}, true},
		{"batch request errored on a bad request", &types.BatchRequestError{Outcome: types.BatchErrored, Err: providerErr(types.ErrorKindInvalidRequest)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsInfra(tt.err); got != tt.want {
				t.Fatalf("IsInfra(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
	if Infra(nil) != nil {
		t.Fatal("Infra(nil) is not nil")
	}
}

func TestMarkSubjectError(t *testing.T) {
	var obs Observation
	MarkSubjectError(&obs, Infra(errors.New("down")))
	if SubjectError(obs) != "down" || !SubjectInconclusive(obs) {
		t.Fatalf("infra error: subject error %q, inconclusive %v", SubjectError(obs), SubjectInconclusive(obs))
	}
	MarkSubjectError(&obs, errors.New("bad output"))
	if SubjectError(obs) != "bad output" || SubjectInconclusive(obs) {
		t.Fatalf("real error kept the inconclusive marker: %+v", obs.Annotations)
	}
	MarkSubjectError(&obs, nil)
	if len(obs.Annotations) != 0 {
		t.Fatalf("nil error left annotations: %+v", obs.Annotations)
	}
	// A marker without a subject error means nothing.
	obs.Annotations = map[string]json.RawMessage{AnnotationSubjectInconclusive: json.RawMessage("true")}
	if SubjectInconclusive(obs) {
		t.Fatal("marker without a subject error reported inconclusive")
	}
}

// failingScorer errors on the observations named in errs.
func failingScorer(name string, errs map[string]error, values map[string]float64) Scorer {
	return NewScorerFunc(name, func(_ context.Context, obs Observation) (Score, error) {
		if err, ok := errs[obs.ID]; ok {
			return Score{}, err
		}
		return Score{Name: name, Value: values[obs.ID]}, nil
	})
}

func TestRunClassifiesSubjectAndScorerFailures(t *testing.T) {
	ctx := context.Background()
	obs := []Observation{{ID: "ok"}, {ID: "down"}, {ID: "broken"}, {ID: "judge-down"}}
	subject := func(_ context.Context, o *Observation) error {
		switch o.ID {
		case "down":
			return &types.ProviderError{Kind: types.ErrorKindRateLimit, Err: errors.New("429")}
		case "broken":
			return errors.New("panic in tool")
		}
		o.Output = []byte(`"x"`)
		return nil
	}
	_ = PopulateAll(ctx, obs, subject)
	judge := failingScorer("judge", map[string]error{"judge-down": &types.ProviderError{Kind: types.ErrorKindUnavailable, Err: errors.New("503")}},
		map[string]float64{"ok": 1})
	suite, err := Run(ctx, "s", obs, []Scorer{judge})
	if err != nil {
		t.Fatal(err)
	}
	if suite.SubjectErrors != 2 || suite.Inconclusive != 2 {
		t.Fatalf("subject errors %d, inconclusive %d; want 2 and 2", suite.SubjectErrors, suite.Inconclusive)
	}
	for _, r := range suite.Results {
		if r.Observation.ID == "judge-down" && (len(r.Scores) != 1 || !r.Scores[0].Inconclusive || r.Scores[0].Error == "") {
			t.Fatalf("judge outage score = %+v", r.Scores)
		}
	}
}

func inconclusiveSuite() *SuiteResult {
	down := Observation{ID: "q3"}
	MarkSubjectError(&down, Infra(errors.New("down")))
	results := []ObservationResult{
		{Observation: Observation{ID: "q1"}, Scores: []Score{{Name: "m", Value: 1}}},
		{Observation: Observation{ID: "q2"}, Scores: []Score{{Name: "m", Error: "503", Inconclusive: true}}},
		{Observation: down},
		{Observation: Observation{ID: "q4"}, Scores: []Score{{Name: "m", Value: 1}}},
	}
	return &SuiteResult{Name: "s", Results: results, Aggregate: Aggregate(results), SubjectErrors: 1, Inconclusive: 2}
}

func TestGateOutcomes(t *testing.T) {
	gate := Assertion{Metric: "m", Op: GTE, Threshold: 1}
	tests := []struct {
		name   string
		mutate func(*SuiteResult)
		policy GatePolicy
		gates  []Assertion
		want   Outcome
	}{
		{"infra only is inconclusive", nil, GatePolicy{}, []Assertion{gate}, OutcomeInconclusive},
		{"tolerated share passes", nil, GatePolicy{MaxInconclusive: 0.5}, []Assertion{gate}, OutcomePassed},
		{"just under the share is inconclusive", nil, GatePolicy{MaxInconclusive: 0.49}, []Assertion{gate}, OutcomeInconclusive},
		{"a real failure wins", func(s *SuiteResult) { s.Results[0].Scores[0].Value = 0 }, GatePolicy{MaxInconclusive: 1}, []Assertion{gate}, OutcomeFailed},
		{"a real subject failure wins", func(s *SuiteResult) {
			o := Observation{ID: "q5"}
			MarkSubjectError(&o, errors.New("bad request"))
			s.Results = append(s.Results, ObservationResult{Observation: o})
			s.SubjectErrors++
		}, GatePolicy{MaxInconclusive: 1}, []Assertion{gate}, OutcomeFailed},
		{"a real scorer failure wins", func(s *SuiteResult) { s.Results[1].Scores[0].Inconclusive = false }, GatePolicy{MaxInconclusive: 1}, []Assertion{gate}, OutcomeFailed},
		{"misspelled metric fails", nil, GatePolicy{MaxInconclusive: 1}, []Assertion{{Metric: "nope", Op: GTE, Threshold: 1}}, OutcomeFailed},
		{"metric missing because nothing was measured", func(s *SuiteResult) { s.Results = s.Results[1:3] }, GatePolicy{}, []Assertion{{Metric: "m", Op: GTE, Threshold: 1, Scope: OnAggregate, Where: Where{}}, {Metric: "other", Op: GTE, Threshold: 1}}, OutcomeInconclusive},
		{"incomplete is inconclusive", func(s *SuiteResult) { s.Results = s.Results[:1]; s.SubjectErrors = 0; s.Incomplete = 3 }, GatePolicy{}, []Assertion{gate}, OutcomeInconclusive},
		{"min pass rate grades only conclusive scores", nil, GatePolicy{MaxInconclusive: 0.5}, []Assertion{{Metric: "m", Op: GTE, Threshold: 1, MinPassRate: 1}}, OutcomePassed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := inconclusiveSuite()
			if tt.mutate != nil {
				tt.mutate(s)
			}
			if got := s.GateWith(tt.policy, tt.gates...); got != tt.want {
				t.Fatalf("outcome = %s, want %s; violations %v", got, tt.want, s.Violations)
			}
		})
	}
}

func TestGateLeavesInconclusiveScoresUngraded(t *testing.T) {
	s := inconclusiveSuite()
	s.GateWith(GatePolicy{MaxInconclusive: 1}, Assertion{Metric: "m", Op: GTE, Threshold: 1})
	if s.Results[1].Scores[0].Passed != nil {
		t.Fatal("an inconclusive score got a verdict")
	}
	var kinds []ViolationKind
	for _, v := range s.Violations {
		kinds = append(kinds, v.Kind)
		if !v.Inconclusive() {
			t.Fatalf("real violation %v", v)
		}
	}
	if len(kinds) != 2 {
		t.Fatalf("violations = %v, want the subject outage and the judge outage", s.Violations)
	}
	if got := s.Violations[0].String(); got != "inconclusive: aggregate: subject failed on infrastructure on 1 observations" {
		t.Fatalf("violation string = %q", got)
	}
	rates := GroupPassRate(s.Results, "m")
	if len(rates) != 1 || rates[0].Stat.N != 2 || rates[0].Stat.Value != 1 {
		t.Fatalf("pass rate = %+v, want 2 of 2 graded", rates)
	}
	if got := PassRate(s.Results, "m", GTE, 1); got != 1 {
		t.Fatalf("PassRate = %v, want 1 (inconclusive left out)", got)
	}
	if got := s.InconclusiveRate(); got != 0.5 {
		t.Fatalf("InconclusiveRate = %v, want 0.5", got)
	}
}

func TestGatePolicyValidate(t *testing.T) {
	for _, v := range []float64{-0.1, 1.1} {
		if (GatePolicy{MaxInconclusive: v}).Validate() == nil {
			t.Errorf("MaxInconclusive %v accepted", v)
		}
	}
	if err := (GatePolicy{MaxInconclusive: 0.2}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareMarksUnmeasuredCasesInconclusive(t *testing.T) {
	gate := Assertion{Metric: "m", Op: GTE, Threshold: 1}
	base := &SuiteResult{Name: "base", Results: []ObservationResult{
		{Observation: Observation{ID: "q1"}, Scores: []Score{{Name: "m", Value: 1}}},
		{Observation: Observation{ID: "q2"}, Scores: []Score{{Name: "m", Value: 1}}},
		{Observation: Observation{ID: "q3"}, Scores: []Score{{Name: "m", Value: 1}}},
		{Observation: Observation{ID: "q4"}, Scores: []Score{{Name: "m", Value: 1}}},
	}}
	exp := inconclusiveSuite()
	base.Gate(gate)
	exp.GateWith(GatePolicy{MaxInconclusive: 1}, gate)
	c := CompareSuites(base, exp)
	status := map[string]CaseStatus{}
	for _, d := range c.Cases {
		status[d.ID] = d.Status
	}
	want := map[string]CaseStatus{"q1": CaseUnchanged, "q2": CaseInconclusive, "q3": CaseInconclusive, "q4": CaseUnchanged}
	for id, s := range want {
		if status[id] != s {
			t.Fatalf("case statuses = %v, want %v", status, want)
		}
	}
	if c.ExpInconclusive != 2 || c.PassStats["m"].Pairs.N() != 2 {
		t.Fatalf("exp inconclusive %d, paired verdicts %d; want 2 and 2", c.ExpInconclusive, c.PassStats["m"].Pairs.N())
	}
	if len(c.Regressions()) != 0 {
		t.Fatalf("outage read as regressions: %+v", c.Regressions())
	}
}
