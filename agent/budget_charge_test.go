package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// pricedProvider reports a rate card, which is what chargeBudget needs to cost
// a call at all. Model() is the CONFIGURED name, standing in for a fallback
// chain reporting its primary while a secondary actually serves the request.
type pricedProvider struct {
	model string
}

func (p *pricedProvider) Name() string  { return "priced" }
func (p *pricedProvider) Model() string { return p.model }
func (p *pricedProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{
		Provider: "priced",
		Model:    p.model,
		Known:    true,
		Pricing:  types.Pricing{InputPerMTok: 3, OutputPerMTok: 15, AsOf: "2026-07-01"},
	}
}
func (p *pricedProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

func chargeFixture(t *testing.T, policy types.BudgetPolicy) (*Agent, *EventStream, *types.Budget) {
	t.Helper()
	budget := types.NewBudget(policy)
	a := NewAgent(AgentConfig{
		Provider:     &pricedProvider{model: "primary-model"},
		SystemPrompt: "sys",
	}, WithBudget(budget))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return a, newEventStream(ctx, cancel), budget
}

func TestCacheHitIsNotCharged(t *testing.T) {
	a, stream, budget := chargeFixture(t, types.BudgetPolicy{Limit: types.USD(5)})

	// A response cache replays the ORIGINAL token counts for observability, but
	// no provider call was made and nothing was billed.
	usage := types.UsageDelta{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, CacheHit: true}
	if err := a.chargeBudget(context.Background(), stream, a.cfg.Provider, usage); err != nil {
		t.Fatalf("cache hit rejected: %v", err)
	}

	if spent := budget.Spent(); spent != 0 {
		t.Errorf("spent %s on a cache hit: a cached run must not report spend it did not incur", spent)
	}
	if got := budget.Usage().Requests; got != 0 {
		t.Errorf("requests = %d, want 0: a cache hit is not a billable provider call", got)
	}
}

func TestLiveUsageIsStillCharged(t *testing.T) {
	// The guard above must not disable charging for ordinary calls.
	a, stream, budget := chargeFixture(t, types.BudgetPolicy{Limit: types.USD(5)})

	usage := types.UsageDelta{PromptTokens: 1_000_000, CompletionTokens: 0}
	if err := a.chargeBudget(context.Background(), stream, a.cfg.Provider, usage); err != nil {
		t.Fatalf("live usage rejected: %v", err)
	}
	if spent := budget.Spent(); spent != types.USD(3) {
		t.Errorf("spent = %s, want 3.00", spent)
	}
}

func TestSpendIsAttributedToTheRespondingModel(t *testing.T) {
	a, stream, budget := chargeFixture(t, types.BudgetPolicy{})

	// What a fallback chain looks like from here: the provider reports the
	// primary's name, the response says a secondary answered.
	usage := types.UsageDelta{PromptTokens: 1_000_000, ResponseModel: "secondary-model"}
	if err := a.chargeBudget(context.Background(), stream, a.cfg.Provider, usage); err != nil {
		t.Fatal(err)
	}

	breakdown := budget.Breakdown()
	if len(breakdown) != 1 {
		t.Fatalf("breakdown has %d entries, want 1", len(breakdown))
	}
	if breakdown[0].Model != "secondary-model" {
		t.Errorf("spend attributed to %q, want secondary-model: crediting the primary makes a breakdown point at a model that did not run", breakdown[0].Model)
	}
}

func TestAttributionFallsBackToTheConfiguredModel(t *testing.T) {
	// Not every adapter reports a response model; the configured one is then
	// the best answer available.
	a, stream, budget := chargeFixture(t, types.BudgetPolicy{})

	usage := types.UsageDelta{PromptTokens: 1_000_000}
	if err := a.chargeBudget(context.Background(), stream, a.cfg.Provider, usage); err != nil {
		t.Fatal(err)
	}

	breakdown := budget.Breakdown()
	if len(breakdown) != 1 || breakdown[0].Model != "primary-model" {
		t.Fatalf("breakdown = %+v, want the configured model", breakdown)
	}
}
