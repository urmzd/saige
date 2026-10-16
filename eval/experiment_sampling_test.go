package eval

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestExperimentSamplesExecuteEvaluationsAndFreezeSetup(t *testing.T) {
	var subjects, evaluations atomic.Int32
	subject := func(_ context.Context, obs *Observation) error {
		subjects.Add(1)
		obs.Output = json.RawMessage(`"fixed output"`)
		obs.Timing = ObservationTiming{TotalMs: 30, TTFTMs: 5, InputTokens: 10, OutputTokens: 2}
		obs.Timing.SetCostUSD(0.02)
		return nil
	}
	scorer := Deterministic(NewScorerFunc("correct", func(ctx context.Context, obs Observation) (Score, error) {
		if string(obs.Output) != `"fixed output"` {
			t.Error("evaluation did not see the fixed trace")
		}
		call := evaluations.Add(1)
		RecordEvaluationUsage(ctx, types.UsageDelta{PromptTokens: 100, CompletionTokens: 20}, types.Pricing{InputPerMTok: 1, OutputPerMTok: 2}, true)
		return Score{Name: "correct", Value: float64(call % 2)}, nil
	}))
	exp := Experiment{Name: "cost-quality", Revision: "experiment@2", Config: json.RawMessage(`{"dataset":"billing@3"}`), Variants: []Variant{{Name: "candidate", Revision: "agent@2", Config: json.RawMessage(`{"model":"small","prompt":"prompt@4"}`), Subject: subject}}, Scorers: []Scorer{scorer}, Sampler: Sampler{N: 5}, Assertions: []Assertion{
		{Metric: "correct", Op: GTE, Threshold: 0.5},
		{Metric: "latency_ms", Op: LTE, Threshold: 40},
		{Metric: "cost_usd", Op: LTE, Threshold: 0.03},
		{Metric: "evaluation_cost_usd", Op: LTE, Threshold: 0.001},
		{Metric: "evaluation_latency_ms", Op: LTE, Threshold: 1000},
	}}
	result, err := exp.Sample(context.Background(), []Observation{{ID: "trace", Input: json.RawMessage(`"question"`)}}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if subjects.Load() != 1 || evaluations.Load() != 3 {
		t.Fatalf("subject=%d evaluation=%d", subjects.Load(), evaluations.Load())
	}
	if len(result.Evaluations) != 3 || result.Metrics["correct"].N != 3 {
		t.Fatalf("trial summary=%+v", result)
	}
	if result.Evaluations[0].Outcome != OutcomePassed || result.Evaluations[1].Outcome != OutcomeFailed {
		t.Fatal("assertions were not executed for each evaluation")
	}
	if result.SubjectCostUSD == nil || *result.SubjectCostUSD != 0.02 {
		t.Fatal("subject cost duplicated across evaluation samples")
	}
	if result.EvaluationCost.N != 3 || math.Abs(result.EvaluationCost.Value-0.00014) > 1e-9 {
		t.Fatalf("evaluation cost=%+v", result.EvaluationCost)
	}
	for i, suite := range result.Evaluations {
		if suite.Setup.Revision != result.Setup.Revision || suite.Revision != result.Setup.Revision || suite.Evaluation != i+1 {
			t.Fatal("trial setup identity lost")
		}
		if suite.Aggregate["latency_ms"] != 30 || suite.Aggregate["ttft_ms"] != 5 || suite.Aggregate["total_tokens"] != 12 || suite.Aggregate["evaluation_tokens"] != 120 {
			t.Fatalf("missing metadata: %+v", suite.Aggregate)
		}
		run := NewRunRecord("trial", suite, nil, Provenance{})
		rebuilt := SuiteFromUnits(run, suite.Units("trial"))
		if rebuilt.Setup.Revision != suite.Setup.Revision || rebuilt.Evaluation != suite.Evaluation || rebuilt.EvaluationTiming.CostUSD == nil {
			t.Fatal("stored trial lost setup or evaluation timing")
		}
		suite.Setup.Variants[0].Config = json.RawMessage(`{"mutated":true}`)
		if string(run.Setup.Variants[0].Config) == `{"mutated":true}` || string(result.Setup.Variants[0].Config) == `{"mutated":true}` {
			t.Fatal("snapshot shared mutable state")
		}
	}
}

func TestExperimentSetupTracksSettingsNotOutcomes(t *testing.T) {
	output := 0.0
	subject := func(_ context.Context, obs *Observation) error { obs.Output, _ = json.Marshal(output); return nil }
	exp := Experiment{Name: "x", Revision: "setup@1", Variants: []Variant{{Name: "v", Revision: "agent@1", Config: json.RawMessage(`{"model":"small"}`), Subject: subject}}, Scorers: []Scorer{outputScorer}}
	dataset := []Observation{{ID: "q", Revision: "dataset@1"}}
	a, err := exp.Run(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	output = 1
	b, err := exp.Run(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	if a.Setup.Revision != b.Setup.Revision || a.Revision != b.Revision {
		t.Fatal("measured outcomes changed setup identity")
	}
	exp.Variants[0].Config = json.RawMessage(`{"model":"large"}`)
	c, err := exp.Run(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	comparison := CompareSuites(a, c)
	if c.Revision == a.Revision || len(comparison.SetupChanges) == 0 || comparison.Deltas["correct"] != 1 {
		t.Fatal("setup changes and impact missing")
	}
	changed, err := exp.Snapshot(dataset, 2)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision == c.Setup.Revision {
		t.Fatal("evaluation count missing from setup")
	}
	dataset[0].Revision = "dataset@2"
	revised, err := exp.Snapshot(dataset, 2)
	if err != nil {
		t.Fatal(err)
	}
	if revised.DatasetRevision == changed.DatasetRevision {
		t.Fatal("dataset revision missing from setup")
	}
}

func TestEvaluatorMissingUsageAndErrors(t *testing.T) {
	for _, fail := range []bool{false, true} {
		scorer := NewScorerFunc("judge", func(ctx context.Context, _ Observation) (Score, error) {
			RecordEvaluationUsage(ctx, types.UsageDelta{PromptTokens: 100}, types.Pricing{InputPerMTok: 1}, true)
			RecordEvaluationUsage(ctx, types.UsageDelta{}, types.Pricing{}, false)
			if fail {
				return Score{}, errors.New("judge failure")
			}
			return Score{Name: "judge", Value: 1}, nil
		})
		suite, err := Run(context.Background(), "usage", []Observation{{ID: "q"}}, []Scorer{scorer}, WithMeasurements())
		if fail && err == nil {
			t.Fatal("metadata masked scorer failure")
		}
		if suite.EvaluationTiming.CostUSD != nil {
			t.Fatal("partial pricing reported as known total")
		}
		if _, ok := suite.Aggregate["evaluation_cost_usd"]; ok {
			t.Fatal("unknown cost treated as free")
		}
		if suite.EvaluationTiming.InputTokens != 100 || suite.EvaluationTiming.TotalMs < 1 {
			t.Fatal("usage or latency missing")
		}
	}
}

func TestSetupExposesChangedJudgeSettings(t *testing.T) {
	gen := &reliabilityGenerator{}
	exp := Experiment{Name: "judge-settings", Variants: []Variant{{Name: "v", Subject: constantSubject(1)}}, Scorers: []Scorer{NewJudgeScorer(gen, WithJudgeRubric("first rubric"))}}
	a, err := exp.Snapshot(nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	exp.Scorers = []Scorer{Named(NewJudgeScorer(gen, WithJudgeRubric("second rubric")), "quality")}
	b, err := exp.Snapshot(nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	changedRubric := false
	for _, change := range DiffSetups(a, b) {
		if change.Field == "scorers[0].config.rubric" {
			changedRubric = true
		}
	}
	if !changedRubric {
		t.Fatalf("rubric change hidden: %+v", DiffSetups(a, b))
	}
}

func TestDeclinedScorerStillRecordsEvaluationCost(t *testing.T) {
	sc := NewScorerFunc("declined", func(ctx context.Context, _ Observation) (Score, error) {
		RecordEvaluationUsage(ctx, types.UsageDelta{PromptTokens: 100}, types.Pricing{InputPerMTok: 1}, true)
		return Score{}, nil
	})
	suite, err := Run(context.Background(), "decline", []Observation{{ID: "q"}}, []Scorer{sc}, WithMeasurements())
	if err != nil {
		t.Fatal(err)
	}
	if suite.EvaluationTiming.CostUSD == nil || math.Abs(*suite.EvaluationTiming.CostUSD-0.0001) > 1e-9 {
		t.Fatalf("declined evaluation cost lost: %+v", suite.EvaluationTiming)
	}
	if suite.Aggregate["evaluation_cost_usd"] != 0.0001 {
		t.Fatal("declined evaluation cost not assertable")
	}
}

func TestEvaluateExistingTraceDoesNotInvokeSubjects(t *testing.T) {
	calls := 0
	e := Experiment{Name: "fixed", Variants: []Variant{{Name: "v", Subject: func(context.Context, *Observation) error { t.Fatal("subject invoked for existing trace"); return nil }}}, Scorers: []Scorer{Deterministic(NewScorerFunc("correct", func(_ context.Context, obs Observation) (Score, error) {
		calls++
		if string(obs.Output) != `1` {
			t.Fatal("trace output lost")
		}
		return Score{Name: "correct", Value: 1}, nil
	}))}}
	trials, err := e.Evaluate(context.Background(), []Observation{{ID: "trace", Output: json.RawMessage(`1`), Timing: ObservationTiming{TotalMs: 42}}}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || trials.Setup.Mode != "fixed_traces" || trials.Setup.SubjectRepeats != 0 || trials.Evaluations[0].Aggregate["latency_ms"] != 42 {
		t.Fatalf("fixed trace evaluation=%+v", trials)
	}
	e.Variants[0].Subject = nil
	if _, err := e.Evaluate(context.Background(), []Observation{{ID: "trace", Output: json.RawMessage(`1`)}}, 2); err != nil {
		t.Fatal(err)
	}
}
