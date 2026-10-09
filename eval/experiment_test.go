package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// constantSubject writes value as the output of every observation.
func constantSubject(value float64) Subject {
	return func(_ context.Context, obs *Observation) error {
		obs.Output, _ = json.Marshal(value)
		return nil
	}
}

func TestExperimentRun(t *testing.T) {
	dataset := []Observation{
		{ID: "q1", Labels: Labels{"topic": "billing", LabelVariant: "ignored"}},
		{ID: "q2", Labels: Labels{"topic": "refunds"}},
	}
	e := Experiment{
		Name:  "retrieval",
		Claim: "retrieval improves correctness",
		Variants: []Variant{
			{Name: "control", Labels: Labels{"tools": "none"}, Subject: constantSubject(0)},
			{Name: "rag", Labels: Labels{"tools": "search"}, Subject: constantSubject(1)},
		},
		Scorers: []Scorer{outputScorer},
		Assertions: []Assertion{
			{Metric: "correct", Op: GTE, Threshold: 1, Where: Where{LabelVariant: "rag"}},
		},
	}
	suite, err := e.Run(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "retrieval" || suite.Claim != e.Claim {
		t.Errorf("name %q claim %q", suite.Name, suite.Claim)
	}
	if len(suite.Results) != 4 {
		t.Fatalf("results = %d, want 2 cases x 2 variants", len(suite.Results))
	}
	for _, r := range suite.Results {
		l := r.Observation.Labels
		if l["topic"] == "" {
			t.Errorf("dataset labels lost: %v", l)
		}
		switch l[LabelVariant] {
		case "control":
			if l["tools"] != "none" {
				t.Errorf("control labels = %v", l)
			}
		case "rag":
			if l["tools"] != "search" {
				t.Errorf("rag labels = %v", l)
			}
		default:
			t.Errorf("variant label not stamped over the dataset label: %v", l)
		}
	}
	if suite.Outcome != OutcomePassed {
		t.Errorf("outcome = %q, violations %+v; the scoped gate should pass", suite.Outcome, suite.Violations)
	}
	if dataset[0].Labels[LabelVariant] != "ignored" {
		t.Error("Run must not modify the dataset")
	}

	cmp, err := CompareVariants(suite.Results, "control")
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp["rag"].DeltaStats["correct"]; d.Delta != 1 || d.N != 2 {
		t.Errorf("rag vs control = %+v, want delta 1 over 2 pairs", d)
	}
}

func TestExperimentRunOutcomes(t *testing.T) {
	variants := []Variant{
		{Name: "low", Subject: constantSubject(0)},
		{Name: "high", Subject: constantSubject(1)},
	}
	tests := []struct {
		name       string
		assertions []Assertion
		opts       []Option
		want       Outcome
	}{
		{"no assertions is unasserted", nil, nil, OutcomeUnasserted},
		{"unscoped gate fails on the low variant", []Assertion{{Metric: "correct", Op: GTE, Threshold: 1}}, nil, OutcomeFailed},
		{"assertions passed as options also gate", nil, []Option{WithAssertions(Assertion{Metric: "correct", Op: LTE, Threshold: 1})}, OutcomePassed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Experiment{Name: "x", Variants: variants, Scorers: []Scorer{outputScorer}, Assertions: tt.assertions}
			suite, err := e.Run(context.Background(), []Observation{{ID: "q"}}, tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if suite.Outcome != tt.want {
				t.Errorf("outcome = %q, want %q", suite.Outcome, tt.want)
			}
		})
	}
}

func TestExperimentRunRepeatsAndConcurrency(t *testing.T) {
	var running, peak, calls atomic.Int32
	subject := Subject(func(_ context.Context, obs *Observation) error {
		calls.Add(1)
		n := running.Add(1)
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		// Sample 2 of q1 fails the check, so q1 passes 2 of 3 samples.
		v := 1.0
		if obs.ID == "q1" && obs.Sample == 2 {
			v = 0
		}
		obs.Output, _ = json.Marshal(v)
		return nil
	})
	e := Experiment{
		Name:       "repeats",
		Variants:   []Variant{{Name: "only", Subject: subject}},
		Scorers:    []Scorer{outputScorer},
		Assertions: []Assertion{{Metric: "correct", Op: GTE, Threshold: 1}},
	}
	suite, err := e.Run(context.Background(), []Observation{{ID: "q1"}, {ID: "q2"}},
		WithRepeats(3), WithConcurrency(4))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 6 || len(suite.Results) != 6 {
		t.Errorf("calls = %d, results = %d, want 6 each", calls.Load(), len(suite.Results))
	}
	if peak.Load() < 2 {
		t.Errorf("subjects should run in parallel, peak %d", peak.Load())
	}
	rates := QuestionPassRates(suite.Results, "correct", 1, 3)
	if len(rates) != 2 {
		t.Fatalf("rates = %+v", rates)
	}
	q1 := rates[0]
	if q1.ID != "q1" || q1.Passed != 2 || q1.Rate.N != 3 || q1.Variant != "only" {
		t.Errorf("q1 = %+v", q1)
	}
	if q1.PassAtK[3] != 1 {
		t.Errorf("q1 pass@3 = %v, want 1", q1.PassAtK[3])
	}
}

func TestExperimentRunSubjectFailureIsolated(t *testing.T) {
	flaky := Subject(func(_ context.Context, obs *Observation) error {
		if obs.ID == "q2" {
			return errors.New("provider 500")
		}
		obs.Output = json.RawMessage(`1`)
		return nil
	})
	e := Experiment{Name: "flaky", Variants: []Variant{{Name: "v", Subject: flaky}}, Scorers: []Scorer{outputScorer}}
	suite, err := e.Run(context.Background(), []Observation{{ID: "q1"}, {ID: "q2"}})
	if err != nil {
		t.Fatal(err)
	}
	if suite.SubjectErrors != 1 || len(suite.Results) != 2 {
		t.Errorf("subject errors = %d, results = %d", suite.SubjectErrors, len(suite.Results))
	}
}

func TestExperimentRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := Experiment{Name: "c", Variants: []Variant{{Name: "v", Subject: constantSubject(1)}}, Scorers: []Scorer{outputScorer}}
	suite, err := e.Run(ctx, []Observation{{ID: "q1"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if suite == nil || suite.Incomplete != 1 {
		t.Errorf("want the partial suite, got %+v", suite)
	}
}

func TestExperimentValidate(t *testing.T) {
	s := constantSubject(1)
	tests := []struct {
		name     string
		variants []Variant
		want     string
	}{
		{"no variants", nil, "no variants"},
		{"unnamed variant", []Variant{{Subject: s}}, "has no name"},
		{"duplicate variant", []Variant{{Name: "a", Subject: s}, {Name: "a", Subject: s}}, "duplicate variant"},
		{"nil subject", []Variant{{Name: "a"}}, "no subject"},
		{"variant label differs from name", []Variant{{Name: "a/x", Labels: Labels{LabelVariant: "a"}, Subject: s}}, "must match the name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Experiment{Name: "x", Variants: tt.variants}.Run(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestExperimentRunSampler(t *testing.T) {
	tests := []struct {
		name    string
		sampler Sampler
		opts    []Option
		wantN   int
	}{
		{"no sampler scores once", Sampler{}, nil, 0},
		{"experiment sampler applies", Sampler{N: 3}, nil, 3},
		{"run option takes precedence", Sampler{N: 3}, []Option{WithSampler(Sampler{N: 2})}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Experiment{
				Name:     "sampled",
				Variants: []Variant{{Name: "a", Subject: constantSubject(1)}},
				Scorers:  []Scorer{outputScorer},
				Sampler:  tt.sampler,
			}
			suite, err := e.Run(context.Background(), []Observation{{ID: "q"}}, tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			if s := suite.Results[0].Scores[0].Samples; s != nil {
				got = len(s.Values)
			}
			if got != tt.wantN {
				t.Errorf("samples = %d, want %d", got, tt.wantN)
			}
		})
	}
}
