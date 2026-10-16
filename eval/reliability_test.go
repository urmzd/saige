package eval

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

type reliabilityGenerator struct {
	calls   int
	prompts []string
}

func (g *reliabilityGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.calls++
	g.prompts = append(g.prompts, prompt)
	switch g.calls {
	case 2:
		return `{"reasoning":"mismatch","score":0.2}`, nil
	case 4:
		return "", errors.New("judge unavailable")
	default:
		return `{"reasoning":"matches desired","score":0.9}`, nil
	}
}
func TestRepeatedJudgeReliability(t *testing.T) {
	gen := &reliabilityGenerator{}
	judge := NewJudgeScorer(gen, WithJudgeRevision("judge-model@2026-10-16"))
	suite, err := Run(context.Background(), "trace", []Observation{{ID: "trace-1", Input: json.RawMessage(`"question"`), Output: json.RawMessage(`"answer"`), GroundTruth: json.RawMessage(`"desired answer"`)}}, []Scorer{judge}, WithSampler(Sampler{N: 5}))
	if err != nil {
		t.Fatal(err)
	}
	if gen.calls != 5 {
		t.Fatalf("calls=%d", gen.calls)
	}
	for _, prompt := range gen.prompts {
		if !strings.Contains(prompt, "desired answer") {
			t.Fatal("desired answer missing")
		}
	}
	score := suite.Results[0].Scores[0]
	if score.Revision == "" || suite.Revision == "" || suite.Results[0].Observation.Revision == "" {
		t.Fatal("missing revisions")
	}
	st := score.Samples
	if st.Interval.N != 4 || st.Interval.Completeness != 0.8 || st.Errors != 1 {
		t.Fatalf("stats=%+v", st)
	}
	if !(st.Interval.Low < st.Mean && st.Interval.High > st.Mean) {
		t.Fatalf("mean interval=%+v", st.Interval)
	}
	r, err := st.Reliability(0.5, false, 1, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	if r.PassRate.Value != 0.75 || r.AgreementRate.Value != 0.25 || r.PassAtK[2] != 1 {
		t.Fatalf("reliability=%+v", r)
	}
	if r.PassRate.N != 4 || r.PassRate.Completeness != 0.8 || !r.PassRate.Contains(0.75) || !r.AgreementRate.Contains(0.25) {
		t.Fatalf("intervals=%+v", r)
	}
	if _, ok := r.PassAtK[5]; ok {
		t.Fatal("undefined pass@5 reported")
	}
	if r.Revision == "" || st.Revision != score.Revision {
		t.Fatal("sample/report revision missing")
	}
	run := NewRunRecord("r1", suite, nil, Provenance{})
	raw, err := json.Marshal(struct {
		Run   RunRecord
		Units []Unit
	}{run, suite.Units(run.ID)})
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Run   RunRecord
		Units []Unit
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	restored := SuiteFromUnits(stored.Run, stored.Units)
	if restored.Revision != suite.Revision || restored.Results[0].Scores[0].Samples.Interval != st.Interval {
		t.Fatal("stored revisions/statistics changed")
	}
}

func TestReliabilityEdgeCases(t *testing.T) {
	for _, values := range [][]float64{nil, {1, 1, 1}, {0, 0, 0}} {
		st := SampleStats{Values: values, Expected: 5, Confidence: 0.99}
		r, err := st.Reliability(0.5, true, 0, 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if r.PassRate.Low < 0 || r.PassRate.High > 1 || r.PassRate.High <= r.PassRate.Low {
			t.Fatalf("invalid Wilson interval: %+v", r)
		}
		if r.Confidence != 0.99 || r.PassRate.N != len(values) {
			t.Fatalf("report=%+v", r)
		}
	}
	if _, err := (SampleStats{}).Reliability(math.NaN(), true); err == nil {
		t.Fatal("accepted NaN threshold")
	}
	if _, err := (SampleStats{Values: []float64{math.Inf(1)}}).Reliability(0.5, true); err == nil {
		t.Fatal("accepted infinite sample")
	}
}

func TestRevisionsTrackConfiguration(t *testing.T) {
	gen := &reliabilityGenerator{}
	base := NewJudgeScorer(gen)
	for _, changed := range []Scorer{
		NewJudgeScorer(gen, WithJudgeRubric("different rubric")),
		NewJudgeScorer(gen, WithJudgeRevision("different model")),
		NewPairwiseJudgeScorer(gen),
	} {
		if ScorerRevision(base) == ScorerRevision(changed) {
			t.Fatal("judge config change did not change revision")
		}
	}
	if ScorerRevision(base) != ScorerRevision(NewJudgeScorer(gen)) {
		t.Fatal("identical config is unstable")
	}
	if ScorerRevision(Named(Deterministic(base), "renamed")) != ScorerRevision(base) {
		t.Fatal("wrapper lost revision")
	}
	if ScorerRevision(JSONFieldScorer("x", 1)) == ScorerRevision(JSONFieldScorer("x", 2)) {
		t.Fatal("field target missing from revision")
	}
	for _, pair := range [][2]Scorer{
		{TokenBudgetScorer(10), TokenBudgetScorer(20)},
		{CostBudgetScorer(0.01), CostBudgetScorer(0.02)},
		{RespondsWithinScorer(10), RespondsWithinScorer(20)},
		{ContainsScorer("a,b"), ContainsScorer("a", "b")},
	} {
		if ScorerRevision(pair[0]) == ScorerRevision(pair[1]) {
			t.Fatal("built-in scorer configuration lost")
		}
	}
	a, err := BuildScorer(ScorerSpec{Kind: "exact_match", Revision: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildScorer(ScorerSpec{Kind: "exact_match", Revision: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if ScorerRevision(a) == ScorerRevision(b) {
		t.Fatal("spec revision ignored")
	}
}

func TestObservationRevisionContentAndTiming(t *testing.T) {
	obs := Observation{ID: "bad-json", Input: json.RawMessage(`broken input`), Output: json.RawMessage(`broken output`)}
	scorer := NewScorerFunc("ok", func(context.Context, Observation) (Score, error) { return Score{Name: "ok", Value: 1}, nil })
	run := func(obs Observation) *SuiteResult {
		t.Helper()
		s, err := Run(context.Background(), "revision", []Observation{obs}, []Scorer{scorer})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := run(obs)
	obs.Timing.TotalMs = 123
	if run(obs).Revision != first.Revision {
		t.Fatal("timing changed revision")
	}
	obs.GroundTruth = json.RawMessage(`"changed desired answer"`)
	if run(obs).Revision == first.Revision {
		t.Fatal("desired answer missing from revision")
	}
	obs.Revision = "dataset@2"
	if run(obs).Results[0].Observation.Revision != "dataset@2" {
		t.Fatal("explicit revision lost")
	}
	if copyObservations([]Observation{obs})[0].Revision != obs.Revision {
		t.Fatal("variant copy lost revision")
	}
}

func TestSampleRevisionTracksReducerAndConfidence(t *testing.T) {
	base := NewScorerFunc("test", func(context.Context, Observation) (Score, error) { return Score{Name: "test", Value: 1}, nil })
	revision := ScorerRevision(Sampled(base, Sampler{N: 5}))
	for _, cfg := range []Sampler{{N: 6}, {N: 5, Confidence: 0.99}, {N: 5, Reduce: Min}} {
		if revision == ScorerRevision(Sampled(base, cfg)) {
			t.Fatal("sampling configuration missing from revision")
		}
	}
	st := SampleStats{Values: []float64{1, 1, 1, 1, 1}, Confidence: 0.95}
	a, err := st.Reliability(0.5, true, 2)
	if err != nil {
		t.Fatal(err)
	}
	st.Confidence = 0.99
	b, err := st.Reliability(0.5, true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b.PassRate.Low >= a.PassRate.Low || a.PassRate.High != 1 || a.PassRate.Value != 1 {
		t.Fatalf("confidence levels not respected: %+v %+v", a, b)
	}
}
