package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunExperiment covers the deprecated names, which must keep working.
func TestRunExperiment(t *testing.T) {
	inputs := []Observation{
		{ID: "e1", Input: json.RawMessage(`"query1"`)},
		{ID: "e2", Input: json.RawMessage(`"query2"`)},
	}

	base := Subject(func(_ context.Context, obs *Observation) error {
		obs.Output = json.RawMessage(`"base response"`)
		obs.Timing.TotalMs = 100
		return nil
	})

	exp := Subject(func(_ context.Context, obs *Observation) error {
		obs.Output = json.RawMessage(`"experimental response"`)
		obs.Timing.TotalMs = 50
		return nil
	})

	latencyScorer := NewScorerFunc("latency_ms", func(_ context.Context, obs Observation) (Score, error) {
		return Score{Name: "latency_ms", Value: float64(obs.Timing.TotalMs)}, nil
	})

	result, err := RunExperiment(context.Background(), inputs, base, exp, []Scorer{latencyScorer},
		WithExperimentName("test-experiment"))
	if err != nil {
		t.Fatal(err)
	}

	if result.Name != "test-experiment" {
		t.Errorf("expected name test-experiment, got %s", result.Name)
	}
	if len(result.BaseResults) != 2 {
		t.Fatalf("expected 2 base results, got %d", len(result.BaseResults))
	}
	if len(result.ExpResults) != 2 {
		t.Fatalf("expected 2 exp results, got %d", len(result.ExpResults))
	}

	// Base latency should be 100, exp should be 50, delta = -50
	if math.Abs(result.BaseAggregate["latency_ms"]-100) > 0.001 {
		t.Errorf("base latency: got %f, want 100", result.BaseAggregate["latency_ms"])
	}
	if math.Abs(result.ExpAggregate["latency_ms"]-50) > 0.001 {
		t.Errorf("exp latency: got %f, want 50", result.ExpAggregate["latency_ms"])
	}
	if math.Abs(result.Deltas["latency_ms"]-(-50)) > 0.001 {
		t.Errorf("delta: got %f, want -50", result.Deltas["latency_ms"])
	}
}

func TestCompareScorerErrorContinues(t *testing.T) {
	inputs := []Observation{
		{ID: "e1", Input: json.RawMessage(`"query1"`)},
		{ID: "e2", Input: json.RawMessage(`"query2"`)},
	}

	subject := Subject(func(_ context.Context, obs *Observation) error {
		obs.Output = json.RawMessage(`"response"`)
		return nil
	})

	// Errors on e1, scores 1.0 on e2.
	flaky := NewScorerFunc("flaky", func(_ context.Context, obs Observation) (Score, error) {
		if obs.ID == "e1" {
			return Score{}, errors.New("boom")
		}
		return Score{Name: "flaky", Value: 1.0}, nil
	})

	result, err := Compare(context.Background(), inputs, subject, subject, []Scorer{flaky})
	if err != nil {
		t.Fatalf("experiment should complete despite scorer error, got %v", err)
	}

	if result.BaseErroredCases != 1 || result.ExpErroredCases != 1 {
		t.Errorf("expected 1 errored case per side, got base=%d exp=%d",
			result.BaseErroredCases, result.ExpErroredCases)
	}
	// Aggregates exclude the errored case: mean over e2 only.
	if math.Abs(result.BaseAggregate["flaky"]-1.0) > 0.001 {
		t.Errorf("base aggregate: got %f, want 1.0", result.BaseAggregate["flaky"])
	}
	if math.Abs(result.Deltas["flaky"]) > 0.001 {
		t.Errorf("delta: got %f, want 0", result.Deltas["flaky"])
	}
}

func experimentInputs(n int) []Observation {
	inputs := make([]Observation, n)
	for i := range inputs {
		inputs[i] = Observation{ID: fmt.Sprintf("c%02d", i), Input: json.RawMessage(fmt.Sprintf("%d", i))}
	}
	return inputs
}

// indexValue scores the observation by its numeric input, scaled into [0, 1],
// plus whatever offset the subject wrote as output.
func indexValue(n int) Scorer {
	return NewScorerFunc("v", func(_ context.Context, obs Observation) (Score, error) {
		var idx int
		var offset float64
		_ = json.Unmarshal(obs.Input, &idx)
		_ = json.Unmarshal(obs.Output, &offset)
		return Score{Name: "v", Value: float64(idx%7)/float64(n) + offset}, nil
	})
}

func offsetSubject(offset float64) Subject {
	return func(_ context.Context, obs *Observation) error {
		obs.Output, _ = json.Marshal(offset)
		return nil
	}
}

func TestCompareDeltaStats(t *testing.T) {
	const n = 50
	tests := []struct {
		name         string
		expOffset    float64
		wantDelta    float64
		wantExcludes bool
	}{
		{name: "identical arms contain zero", expOffset: 0, wantDelta: 0},
		{name: "uniform gain excludes zero", expOffset: 0.3, wantDelta: 0.3, wantExcludes: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Compare(context.Background(), experimentInputs(n),
				offsetSubject(0), offsetSubject(tt.expOffset), []Scorer{indexValue(n)})
			if err != nil {
				t.Fatal(err)
			}
			stats, ok := result.DeltaStats["v"]
			if !ok {
				t.Fatal("missing DeltaStats for v")
			}
			if stats.N != n {
				t.Errorf("N: got %d, want %d", stats.N, n)
			}
			assertClose(t, "delta", stats.Delta, tt.wantDelta, 1e-9)
			containsZero := stats.CILow <= 0 && stats.CIHigh >= 0
			if containsZero == tt.wantExcludes {
				t.Errorf("CI [%g, %g]: contains zero = %v", stats.CILow, stats.CIHigh, containsZero)
			}
			if stats.CILow > stats.Delta || stats.CIHigh < stats.Delta {
				t.Errorf("CI [%g, %g] does not contain delta %g", stats.CILow, stats.CIHigh, stats.Delta)
			}
			if result.BaseCounts["v"] != n || result.ExpCounts["v"] != n {
				t.Errorf("counts: base %d exp %d", result.BaseCounts["v"], result.ExpCounts["v"])
			}
		})
	}
}

func TestCompareNoisyDeltaContainsZero(t *testing.T) {
	// Exp alternates +0.2 and -0.2 around base: the mean delta is 0 and the
	// interval must straddle it.
	const n = 40
	exp := Subject(func(_ context.Context, obs *Observation) error {
		var idx int
		_ = json.Unmarshal(obs.Input, &idx)
		offset := 0.2
		if idx%2 == 1 {
			offset = -0.2
		}
		obs.Output, _ = json.Marshal(offset)
		return nil
	})
	result, err := Compare(context.Background(), experimentInputs(n), offsetSubject(0), exp, []Scorer{indexValue(n)})
	if err != nil {
		t.Fatal(err)
	}
	stats := result.DeltaStats["v"]
	if stats.CILow > 0 || stats.CIHigh < 0 {
		t.Errorf("CI [%g, %g] should contain 0", stats.CILow, stats.CIHigh)
	}
}

func TestCompareMissingMetric(t *testing.T) {
	inputs := experimentInputs(3)
	onlyExp := NewScorerFunc("only_exp", func(_ context.Context, obs Observation) (Score, error) {
		if string(obs.Output) == `"base"` {
			return Score{}, errors.New("base cannot be scored")
		}
		return Score{Name: "only_exp", Value: 0.9}, nil
	})
	onlyBase := NewScorerFunc("only_base", func(_ context.Context, obs Observation) (Score, error) {
		if string(obs.Output) == `"base"` {
			return Score{Name: "only_base", Value: 0.4}, nil
		}
		return Score{}, nil
	})
	base := Subject(func(_ context.Context, obs *Observation) error {
		obs.Output = json.RawMessage(`"base"`)
		return nil
	})
	exp := Subject(func(_ context.Context, obs *Observation) error {
		obs.Output = json.RawMessage(`"exp"`)
		return nil
	})

	result, err := Compare(context.Background(), inputs, base, exp, []Scorer{onlyExp, onlyBase})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Deltas["only_exp"]; ok {
		t.Error("a metric absent from base must not appear in Deltas")
	}
	if len(result.MissingInBase) != 1 || result.MissingInBase[0] != "only_exp" {
		t.Errorf("MissingInBase: got %v", result.MissingInBase)
	}
	if len(result.MissingInExp) != 1 || result.MissingInExp[0] != "only_base" {
		t.Errorf("MissingInExp: got %v", result.MissingInExp)
	}
}

func TestCompareSubjectFailureIsolated(t *testing.T) {
	inputs := experimentInputs(10)
	var running, peak atomic.Int32
	exp := Subject(func(_ context.Context, obs *Observation) error {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if obs.ID == "c04" {
			return errors.New("provider 400")
		}
		obs.Output = json.RawMessage(`0`)
		return nil
	})

	result, err := Compare(context.Background(), inputs, offsetSubject(0), exp,
		[]Scorer{indexValue(10)}, WithConcurrency(4))
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Errorf("subjects should run in parallel, peak %d", peak.Load())
	}
	if result.ExpSubjectErrors != 1 {
		t.Errorf("ExpSubjectErrors: got %d, want 1", result.ExpSubjectErrors)
	}
	if result.ExpCounts["v"] != 9 {
		t.Errorf("exp scored cases: got %d, want 9", result.ExpCounts["v"])
	}
	var failed *ObservationResult
	for i := range result.ExpResults {
		if result.ExpResults[i].Observation.ID == "c04" {
			failed = &result.ExpResults[i]
		}
	}
	if failed == nil || SubjectError(failed.Observation) != "provider 400" {
		t.Errorf("failed case must be kept with its subject error, got %+v", failed)
	}
	if result.DeltaStats["v"].N != 9 {
		t.Errorf("paired N: got %d, want 9", result.DeltaStats["v"].N)
	}
}

func TestCompareCancelledReturnsPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Compare(ctx, experimentInputs(3), offsetSubject(0), offsetSubject(0), []Scorer{indexValue(3)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if result == nil || result.BaseIncomplete != 3 || result.ExpIncomplete != 3 {
		t.Errorf("expected partial result with incomplete counts, got %+v", result)
	}
}

func TestCopyObservationsKeepsSampleAndAnnotations(t *testing.T) {
	in := []Observation{{
		ID:          "a",
		Turn:        2,
		Sample:      3,
		Input:       json.RawMessage(`"q"`),
		Annotations: map[string]json.RawMessage{"context": json.RawMessage(`"ctx"`)},
		Output:      json.RawMessage(`"stale"`),
	}}
	out := copyObservations(in)
	if out[0].Sample != 3 || out[0].Turn != 2 {
		t.Errorf("Sample/Turn not copied: %+v", out[0])
	}
	if string(out[0].Annotations["context"]) != `"ctx"` {
		t.Errorf("annotations not copied: %v", out[0].Annotations)
	}
	if out[0].Output != nil {
		t.Errorf("output must not be copied, got %s", out[0].Output)
	}
	out[0].Annotations["context"][1] = 'X'
	out[0].Annotations["new"] = json.RawMessage(`1`)
	if string(in[0].Annotations["context"]) != `"ctx"` || len(in[0].Annotations) != 1 {
		t.Error("copy must not alias the input annotations")
	}
}

func TestCompareDefaultNames(t *testing.T) {
	tests := []struct {
		name string
		run  func() (*Comparison, error)
		want string
	}{
		{"compare", func() (*Comparison, error) {
			return Compare(context.Background(), experimentInputs(2), offsetSubject(0), offsetSubject(0), []Scorer{indexValue(2)})
		}, "comparison"},
		{"deprecated run experiment", func() (*Comparison, error) {
			return RunExperiment(context.Background(), experimentInputs(2), offsetSubject(0), offsetSubject(0), []Scorer{indexValue(2)})
		}, "experiment"},
		{"explicit name", func() (*Comparison, error) {
			return Compare(context.Background(), experimentInputs(2), offsetSubject(0), offsetSubject(0), []Scorer{indexValue(2)}, WithName("mine"))
		}, "mine"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.run()
			if err != nil {
				t.Fatal(err)
			}
			if c.Name != tt.want {
				t.Errorf("Name = %q, want %q", c.Name, tt.want)
			}
		})
	}
}

// boolSubject writes whether the case passes in the arm: pass(id) decides.
func boolSubject(pass func(id string, sample int) bool) Subject {
	return func(_ context.Context, obs *Observation) error {
		v := 0.0
		if pass(obs.ID, obs.Sample) {
			v = 1
		}
		obs.Output, _ = json.Marshal(v)
		return nil
	}
}

var outputScorer = NewScorerFunc("correct", func(_ context.Context, obs Observation) (Score, error) {
	var v float64
	if err := json.Unmarshal(obs.Output, &v); err != nil {
		return Score{}, err
	}
	return Score{Name: "correct", Value: v}, nil
})

func TestComparePassStatsMcNemar(t *testing.T) {
	const n = 50
	inputs := experimentInputs(n)
	gate := WithAssertions(Assertion{Metric: "correct", Op: GTE, Threshold: 1})
	tests := []struct {
		name         string
		base, exp    func(string, int) bool
		wantPairs    [4]int // both, onlyBase, onlyExp, neither
		excludesZero bool
	}{
		{
			name:         "exp fixes ten cases",
			base:         func(id string, _ int) bool { return id >= "c10" },
			exp:          func(string, int) bool { return true },
			wantPairs:    [4]int{40, 0, 10, 0},
			excludesZero: true,
		},
		{
			name:      "arms swap two cases",
			base:      func(id string, _ int) bool { return id != "c00" },
			exp:       func(id string, _ int) bool { return id != "c01" },
			wantPairs: [4]int{48, 1, 1, 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Compare(context.Background(), inputs, boolSubject(tt.base), boolSubject(tt.exp), []Scorer{outputScorer}, gate)
			if err != nil {
				t.Fatal(err)
			}
			ps, ok := c.PassStats["correct"]
			if !ok {
				t.Fatalf("no pass stats: %+v", c.PassStats)
			}
			got := [4]int{ps.Pairs.Both, ps.Pairs.OnlyA, ps.Pairs.OnlyB, ps.Pairs.Neither}
			if got != tt.wantPairs {
				t.Errorf("pairs = %v, want %v", got, tt.wantPairs)
			}
			if excl := !ps.Diff.Contains(0); excl != tt.excludesZero {
				t.Errorf("diff %+v excludes zero = %v, want %v", ps.Diff, excl, tt.excludesZero)
			}
			if tt.excludesZero && ps.McNemarP >= 0.05 {
				t.Errorf("McNemarP = %v, want < 0.05", ps.McNemarP)
			}
			if c.BaseOutcome == "" || c.ExpOutcome == "" {
				t.Errorf("arm outcomes not recorded: %q %q", c.BaseOutcome, c.ExpOutcome)
			}
		})
	}
}

func TestComparePassStatsAbsentWithoutGate(t *testing.T) {
	c, err := Compare(context.Background(), experimentInputs(3), offsetSubject(0), offsetSubject(0), []Scorer{indexValue(3)})
	if err != nil {
		t.Fatal(err)
	}
	if c.PassStats != nil {
		t.Errorf("PassStats = %+v, want nil without assertions", c.PassStats)
	}
}

func scored(id string, turn, sample int, metric string, value float64, passed *bool) ObservationResult {
	return ObservationResult{
		Observation: Observation{ID: id, Turn: turn, Sample: sample},
		Scores:      []Score{{Name: metric, Value: value, Passed: passed}},
	}
}

func ptr[T any](v T) *T { return &v }

func TestCompareSuitesCaseDiff(t *testing.T) {
	tests := []struct {
		name   string
		base   []ObservationResult
		exp    []ObservationResult
		opts   []Option
		want   map[string]CaseStatus // case ID -> status
		regLen int
	}{
		{
			name:   "values higher is better",
			base:   []ObservationResult{scored("a", 0, 0, "m", 0.5, nil), scored("b", 0, 0, "m", 0.5, nil), scored("c", 0, 0, "m", 0.5, nil)},
			exp:    []ObservationResult{scored("a", 0, 0, "m", 0.9, nil), scored("b", 0, 0, "m", 0.1, nil), scored("c", 0, 0, "m", 0.5, nil)},
			want:   map[string]CaseStatus{"a": CaseImproved, "b": CaseRegressed, "c": CaseUnchanged},
			regLen: 1,
		},
		{
			name: "lower is better flips direction",
			base: []ObservationResult{scored("a", 0, 0, "latency", 100, nil)},
			exp:  []ObservationResult{scored("a", 0, 0, "latency", 50, nil)},
			opts: []Option{WithLowerIsBetter("latency")},
			want: map[string]CaseStatus{"a": CaseImproved},
		},
		{
			name:   "pass verdicts decide over values",
			base:   []ObservationResult{scored("a", 0, 0, "m", 0.9, ptr(true))},
			exp:    []ObservationResult{scored("a", 0, 0, "m", 0.95, ptr(false))},
			want:   map[string]CaseStatus{"a": CaseRegressed},
			regLen: 1,
		},
		{
			name:   "samples roll up into a pass rate",
			base:   []ObservationResult{scored("a", 0, 1, "m", 1, ptr(true)), scored("a", 0, 2, "m", 1, ptr(true))},
			exp:    []ObservationResult{scored("a", 0, 1, "m", 1, ptr(true)), scored("a", 0, 2, "m", 0, ptr(false))},
			want:   map[string]CaseStatus{"a": CaseRegressed},
			regLen: 1,
		},
		{
			name: "one-sided cases",
			base: []ObservationResult{scored("gone", 0, 0, "m", 1, nil)},
			exp:  []ObservationResult{scored("new", 0, 0, "m", 1, nil)},
			want: map[string]CaseStatus{"gone": CaseOnlyBase, "new": CaseOnlyExp},
		},
		{
			name: "errored exp score is only base",
			base: []ObservationResult{scored("a", 0, 0, "m", 1, nil)},
			exp:  []ObservationResult{{Observation: Observation{ID: "a"}, Scores: []Score{{Name: "m", Error: "boom"}}}},
			want: map[string]CaseStatus{"a": CaseOnlyBase},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &SuiteResult{Name: "baseline", Results: tt.base}
			exp := &SuiteResult{Name: "current", Results: tt.exp}
			c := CompareSuites(base, exp, tt.opts...)
			if c.Base != "baseline" || c.Exp != "current" || c.Name != "current" {
				t.Errorf("names = %q %q %q", c.Base, c.Exp, c.Name)
			}
			got := map[string]CaseStatus{}
			for _, d := range c.Cases {
				got[d.ID] = d.Status
			}
			if len(got) != len(tt.want) {
				t.Fatalf("cases = %+v, want %v", c.Cases, tt.want)
			}
			for id, st := range tt.want {
				if got[id] != st {
					t.Errorf("case %s status = %q, want %q", id, got[id], st)
				}
			}
			if len(c.Regressions()) != tt.regLen {
				t.Errorf("Regressions = %+v, want %d", c.Regressions(), tt.regLen)
			}
		})
	}
}

func TestCompareSuitesAgainstSavedBaseline(t *testing.T) {
	assertion := Assertion{Metric: "correct", Op: GTE, Threshold: 1}
	baseline, err := Run(context.Background(), "nightly", []Observation{
		{ID: "a", Output: json.RawMessage(`1`)},
		{ID: "b", Output: json.RawMessage(`1`)},
	}, []Scorer{outputScorer}, WithAssertions(assertion))
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/baseline.json"
	if err := WriteSuiteResult(path, baseline); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadSuiteResult(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := Run(context.Background(), "nightly", []Observation{
		{ID: "a", Output: json.RawMessage(`1`)},
		{ID: "b", Output: json.RawMessage(`0`)},
	}, []Scorer{outputScorer}, WithAssertions(assertion))
	if err != nil {
		t.Fatal(err)
	}
	c := CompareSuites(saved, current)
	regs := c.Regressions()
	if len(regs) != 1 || regs[0].ID != "b" {
		t.Fatalf("regressions = %+v, want case b", regs)
	}
	if ps := c.PassStats["correct"]; ps.Pairs.OnlyA != 1 || ps.Pairs.Both != 1 {
		t.Errorf("pass pairs = %+v, want both=1 only_base=1", ps.Pairs)
	}
}

func TestCompareVariants(t *testing.T) {
	results := []ObservationResult{}
	add := func(variant, id string, v float64) {
		r := scored(id, 0, 0, "m", v, nil)
		r.Observation.Labels = Labels{LabelVariant: variant}
		results = append(results, r)
	}
	for _, id := range []string{"a", "b"} {
		add("control", id, 0.5)
		add("better", id, 0.9)
		add("worse", id, 0.1)
	}

	got, err := CompareVariants(results, "control")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		variant string
		delta   float64
	}{
		{"better", 0.4},
		{"worse", -0.4},
	}
	if len(got) != len(tests) {
		t.Fatalf("comparisons = %d, want %d", len(got), len(tests))
	}
	for _, tt := range tests {
		c := got[tt.variant]
		if c == nil {
			t.Fatalf("no comparison for %s", tt.variant)
		}
		if c.Base != "control" || c.Exp != tt.variant {
			t.Errorf("arms = %q vs %q", c.Base, c.Exp)
		}
		if math.Abs(c.DeltaStats["m"].Delta-tt.delta) > 1e-9 || c.DeltaStats["m"].N != 2 {
			t.Errorf("%s delta = %+v, want %v over 2 pairs", tt.variant, c.DeltaStats["m"], tt.delta)
		}
	}

	if _, err := CompareVariants(results, "missing"); err == nil {
		t.Error("missing baseline variant should error")
	}
}

func TestCompareRepeats(t *testing.T) {
	var calls atomic.Int32
	subject := Subject(func(_ context.Context, obs *Observation) error {
		calls.Add(1)
		obs.Output = json.RawMessage(`1`)
		return nil
	})
	c, err := Compare(context.Background(), experimentInputs(2), subject, subject, []Scorer{outputScorer},
		WithRepeats(3), WithConcurrency(2))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 12 {
		t.Errorf("subject calls = %d, want 2 inputs x 3 repeats x 2 arms = 12", calls.Load())
	}
	samples := map[int]bool{}
	for _, r := range c.ExpResults {
		samples[r.Observation.Sample] = true
	}
	if len(samples) != 3 || !samples[1] || !samples[3] {
		t.Errorf("samples = %v, want 1..3", samples)
	}
	if c.DeltaStats["correct"].N != 2 {
		t.Errorf("paired N = %d, want 2 cases", c.DeltaStats["correct"].N)
	}
}

func TestCopyObservationsClonesLabels(t *testing.T) {
	in := []Observation{{ID: "a", Labels: Labels{"topic": "billing"}}}
	out := copyObservations(in)
	out[0].Labels["topic"] = "changed"
	if in[0].Labels["topic"] != "billing" {
		t.Error("copy must not alias the input labels")
	}
}

func TestCompareSuitesMultiVariant(t *testing.T) {
	// a holds steady between runs, b regresses on q2.
	run := func(bOnQ2 float64) *SuiteResult {
		subject := func(variant string) Subject {
			return func(_ context.Context, obs *Observation) error {
				v := 1.0
				if variant == "b" && obs.ID == "q2" {
					v = bOnQ2
				}
				obs.Output, _ = json.Marshal(v)
				return nil
			}
		}
		e := Experiment{
			Name: "run",
			Variants: []Variant{
				{Name: "a", Subject: subject("a")},
				{Name: "b", Subject: subject("b")},
			},
			Scorers:    []Scorer{outputScorer},
			Assertions: []Assertion{{Metric: "correct", Op: GTE, Threshold: 1}},
		}
		suite, err := e.Run(context.Background(), []Observation{{ID: "q1"}, {ID: "q2"}})
		if err != nil {
			t.Fatal(err)
		}
		return suite
	}
	c := CompareSuites(run(1), run(0))

	if d := c.DeltaStats["correct"]; d.N != 4 || math.Abs(d.Delta+0.25) > 1e-9 {
		t.Errorf("delta = %+v, want -0.25 over 4 pairs", d)
	}
	if p := c.PassStats["correct"].Pairs; p.N() != 4 || p.Both != 3 || p.OnlyA != 1 {
		t.Errorf("pass pairs = %+v, want 4 pairs, both=3 only_base=1", p)
	}
	if len(c.Cases) != 4 {
		t.Fatalf("cases = %d, want one per variant and question", len(c.Cases))
	}
	tests := []struct {
		variant, id string
		want        CaseStatus
	}{
		{"a", "q1", CaseUnchanged},
		{"a", "q2", CaseUnchanged},
		{"b", "q1", CaseUnchanged},
		{"b", "q2", CaseRegressed},
	}
	for _, tt := range tests {
		var found bool
		for _, d := range c.Cases {
			if d.Variant == tt.variant && d.ID == tt.id {
				found = true
				if d.Status != tt.want {
					t.Errorf("%s/%s status = %q, want %q", tt.variant, tt.id, d.Status, tt.want)
				}
			}
		}
		if !found {
			t.Errorf("no case diff for %s/%s", tt.variant, tt.id)
		}
	}

	// The on-disk form pairs each variant with itself, so both arms share
	// four case numbers.
	dir := t.TempDir()
	if err := WriteComparison(dir, c); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		var base, exp ObservationResult
		name := fmt.Sprintf("%03d.json", i)
		if err := readJSON(dir+"/outputs/base/"+name, &base); err != nil {
			t.Fatal(err)
		}
		if err := readJSON(dir+"/outputs/exp/"+name, &exp); err != nil {
			t.Fatal(err)
		}
		if keyWithVariant(base.Observation) != keyWithVariant(exp.Observation) {
			t.Errorf("%s pairs %+v with %+v", name, base.Observation, exp.Observation)
		}
	}
}

func TestCompareVariantsOutcomes(t *testing.T) {
	results := []ObservationResult{}
	add := func(variant, id string, v float64) {
		r := scored(id, 0, 0, "m", v, nil)
		r.Observation.Labels = Labels{LabelVariant: variant}
		results = append(results, r)
	}
	// control passes 1 of 2 cases at threshold 1 with mean 0.5; cand
	// passes both.
	add("control", "a", 1)
	add("control", "b", 0)
	add("cand", "a", 1)
	add("cand", "b", 1)

	tests := []struct {
		name               string
		assertions         []Assertion
		wantBase, wantExpt Outcome
	}{
		{"no assertions leaves outcomes empty", nil, "", ""},
		{"min pass rate met by half", []Assertion{{Metric: "m", Op: GTE, Threshold: 1, MinPassRate: 0.5}}, OutcomePassed, OutcomePassed},
		{"every case", []Assertion{{Metric: "m", Op: GTE, Threshold: 1}}, OutcomeFailed, OutcomePassed},
		{"aggregate only", []Assertion{{Metric: "m", Op: GTE, Threshold: 0.75, Scope: OnAggregate}}, OutcomeFailed, OutcomePassed},
		{"aggregate scoped to the candidate", []Assertion{{Metric: "m", Op: LTE, Threshold: 0.5, Scope: OnAggregate, Where: Where{LabelVariant: "cand"}}}, OutcomeUnasserted, OutcomeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareVariants(results, "control", WithAssertions(tt.assertions...))
			if err != nil {
				t.Fatal(err)
			}
			c := got["cand"]
			if c.BaseOutcome != tt.wantBase || c.ExpOutcome != tt.wantExpt {
				t.Errorf("outcomes = %q / %q, want %q / %q", c.BaseOutcome, c.ExpOutcome, tt.wantBase, tt.wantExpt)
			}
			for _, r := range results {
				if r.Scores[0].Passed != nil {
					t.Fatalf("CompareVariants modified the input results: %+v", r)
				}
			}
		})
	}
}

// TestCompareRepeatsPairByCase checks that repeating every case does not
// shrink the paired interval or inflate N: repeats are averaged within their
// case, and the case is the paired unit.
func TestCompareRepeatsPairByCase(t *testing.T) {
	build := func(repeats int) (base, exp []ObservationResult) {
		for i := range 10 {
			id := fmt.Sprintf("c%02d", i)
			for s := 1; s <= repeats; s++ {
				expVal, expPass := 0.0, false
				if i == 0 {
					expVal, expPass = 1, true // the only case that improves
				}
				base = append(base, scored(id, 0, s, "m", 0, ptr(false)))
				exp = append(exp, scored(id, 0, s, "m", expVal, ptr(expPass)))
			}
		}
		return base, exp
	}
	for _, repeats := range []int{1, 5} {
		t.Run(fmt.Sprintf("repeats=%d", repeats), func(t *testing.T) {
			base, exp := build(repeats)
			c := CompareSuites(&SuiteResult{Name: "b", Results: base}, &SuiteResult{Name: "e", Results: exp})
			d := c.DeltaStats["m"]
			if d.N != 10 || math.Abs(d.Delta-0.1) > 1e-9 {
				t.Errorf("delta = %+v, want 0.1 over 10 cases", d)
			}
			if d.CILow > 0 {
				t.Errorf("interval [%v, %v] excludes 0; one improved case of ten is not a separation", d.CILow, d.CIHigh)
			}
			ps := c.PassStats["m"]
			if ps.Diff.N != 10 || math.Abs(ps.Diff.Value-0.1) > 1e-9 || ps.CaseRates != (repeats > 1) {
				t.Errorf("pass stats = %+v, want diff 0.1 over 10 cases, case rates %v", ps, repeats > 1)
			}
			if ps.Diff.Low > 0 {
				t.Errorf("pass diff interval [%v, %v] excludes 0", ps.Diff.Low, ps.Diff.High)
			}
		})
	}
}
