package online_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/memstore"
	"github.com/urmzd/saige/internal/must"
)

// TestLiveJudgeOnRecordedConversation scores the recorded support
// conversation with an LLM judge on gpt-6-luna, charged to a budget. It runs
// only with SAIGE_LIVE=1 and an OPENAI_API_KEY.
func TestLiveJudgeOnRecordedConversation(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if os.Getenv("SAIGE_LIVE") != "1" || key == "" {
		t.Skip("set SAIGE_LIVE=1 and OPENAI_API_KEY to call the provider")
	}
	tr, _ := supportConversation(t)
	budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01), MaxRequests: 4})
	judge := eval.NewJudgeScorer(&online.BudgetedGenerator{Provider: must.Get(openai.New(openai.Config{APIKey: key, Model: "gpt-6-luna"})), Budget: budget},
		eval.WithJudgeName("helpful"),
		eval.WithJudgeRubric("Score 1 when the response answers the user's request or honestly explains why it could not, 0 when it does neither."))
	ms := memstore.New()
	s := &online.Sampler{Store: ms, Judges: []eval.Scorer{judge}, Budget: budget}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := s.Sweep(ctx, online.TreeSource{"conv-1": tr}, online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.JudgesSkipped != 0 {
		t.Fatalf("judges skipped %d times; budget %+v", rep.JudgesSkipped, budget.Breakdown())
	}
	units, err := ms.Units(ctx, rep.Run.ID, store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		if len(u.Scores) != 1 || u.Scores[0].Error != "" {
			t.Fatalf("unit %s scores = %+v", u.Key, u.Scores)
		}
		t.Logf("%s: helpful=%.2f (%s)", u.Observation.Labels[online.LabelErrored], u.Scores[0].Value, u.Scores[0].Reason)
	}
	if used := budget.Usage(); used.Requests != 2 || budget.Spent() <= 0 {
		t.Fatalf("budget charged %d requests, spent %s; want 2 priced calls", used.Requests, budget.Spent())
	}
	t.Logf("judge spend %s over %d calls", budget.Spent(), budget.Usage().Requests)
}
