package eval

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

type pricedJudge struct{ calls atomic.Int32 }

func (*pricedJudge) Capabilities() types.ModelCapabilities {
	o := textOffering()
	return types.ModelCapabilities{Offering: &o, Pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 2}}
}
func (p *pricedJudge) Stream(context.Context, types.Request) (<-chan types.Delta, error) {
	p.calls.Add(1)
	ch := make(chan types.Delta, 3)
	ch <- types.PartDelta{Index: 0, Text: `{"reasoning":"matches","score":1}`}
	ch <- types.UsageDelta{PromptTokens: 100}
	ch <- types.UsageDelta{CompletionTokens: 20}
	close(ch)
	return ch, nil
}
func TestExperimentSamplingCapturesRealGeneratorUsage(t *testing.T) {
	p := &pricedJudge{}
	var subjects atomic.Int32
	exp := topeval.Experiment{Name: "usage", Variants: []topeval.Variant{{Name: "v", Subject: func(_ context.Context, o *topeval.Observation) error {
		subjects.Add(1)
		o.Output = json.RawMessage(`"answer"`)
		return nil
	}}}, Scorers: []topeval.Scorer{topeval.NewJudgeScorer(NewGenerator(p))}, Sampler: topeval.Sampler{N: 4, Concurrency: 2}}
	result, err := exp.Sample(context.Background(), []topeval.Observation{{ID: "q"}}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if subjects.Load() != 1 || p.calls.Load() != 12 {
		t.Fatalf("subjects=%d judge calls=%d", subjects.Load(), p.calls.Load())
	}
	for _, trial := range result.Evaluations {
		if trial.Setup.Scorers[0].Revision != trial.Results[0].Scores[0].Revision {
			t.Fatal("setup and executed scorer revisions differ")
		}
		if trial.EvaluationTiming.InputTokens != 400 || trial.EvaluationTiming.OutputTokens != 80 {
			t.Fatalf("usage=%+v", trial.EvaluationTiming)
		}
		if trial.EvaluationTiming.CostUSD == nil || *trial.EvaluationTiming.CostUSD < 0.000559 || *trial.EvaluationTiming.CostUSD > 0.000561 {
			t.Fatalf("cost=%+v", trial.EvaluationTiming)
		}
	}
}
