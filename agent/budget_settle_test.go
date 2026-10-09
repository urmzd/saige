package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// pricedStepProvider is a stepProvider with a rate card: $3 per million
// prompt tokens and $15 per million completion tokens.
type pricedStepProvider struct{ *stepProvider }

func (pricedStepProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "priced", Model: "m", Known: true,
		Pricing: types.Pricing{InputPerMTok: 3, OutputPerMTok: 15, AsOf: "2026-07-01"}}
}

// TestBudgetOvershootFollowsPolicy checks that one call costing more than
// the remaining allowance, which is its reservation when no per-call bound
// is set, is handled by OnExceed instead of failing the step outright.
func TestBudgetOvershootFollowsPolicy(t *testing.T) {
	// One million prompt tokens cost $3 against a $1 limit.
	expensive := append([]types.Delta{types.UsageDelta{PromptTokens: 1_000_000}}, agenttest.TextResponse("answer")...)
	tests := []struct {
		name     string
		onExceed types.BudgetAction
		approve  bool
		wantErr  error
		wantText bool
	}{
		{name: "warn keeps the answer", onExceed: types.BudgetWarn, wantText: true},
		{name: "stop ends the run", onExceed: types.BudgetStop, wantErr: types.ErrBudgetExceeded, wantText: true},
		{name: "approval asks and continues", onExceed: types.BudgetRequireApproval, approve: true, wantText: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := pricedStepProvider{newStepProvider(stepCall{before: expensive})}
			budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), OnExceed: tt.onExceed})
			a := NewAgent(AgentConfig{Provider: provider}, WithBudget(budget))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("go")})
			var text string
			markers := 0
			for d := range stream.Deltas() {
				switch v := d.(type) {
				case types.TextContentDelta:
					text += v.Content
				case types.MarkerDelta:
					markers++
					stream.ResolveMarker(v.ToolCallID, tt.approve, nil)
				}
			}
			err := stream.Wait()
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("Wait = %v, want %v", err, tt.wantErr)
			}
			if (text == "answer") != tt.wantText {
				t.Errorf("text = %q", text)
			}
			if wantMarkers := map[bool]int{true: 1, false: 0}[tt.onExceed == types.BudgetRequireApproval]; markers != wantMarkers {
				t.Errorf("approval markers = %d, want %d", markers, wantMarkers)
			}
			if spent := budget.Spent(); spent != types.USD(3) {
				t.Errorf("spent = %s, want the call's real cost 3.00", spent)
			}
		})
	}
}

// TestInterruptChargesUsedTokens checks that a call stopped by
// SubmitInterruptReplace is charged for what it used, not for its whole
// reservation, so the replacement turn still has budget to run.
func TestInterruptChargesUsedTokens(t *testing.T) {
	tests := []struct {
		name   string
		before []types.Delta
	}{
		{name: "usage reported before the interrupt", before: []types.Delta{
			types.UsageDelta{PromptTokens: 1000}, types.TextStartDelta{}, types.TextContentDelta{Content: "half"},
		}},
		{name: "no usage reported yet", before: []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "half"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := pricedStepProvider{newStepProvider(
				stepCall{before: tt.before, hold: make(chan struct{})},
				stepCall{before: append([]types.Delta{types.UsageDelta{PromptTokens: 10, CompletionTokens: 2}}, agenttest.TextResponse("replaced")...)},
			)}
			budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(5)})
			a := NewAgent(AgentConfig{Provider: provider}, WithBudget(budget))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("start")})
			submitted := false
			var text string
			seen := 0
			for d := range stream.Deltas() {
				if _, usage := d.(types.UsageDelta); !usage {
					seen++
				}
				if v, ok := d.(types.TextContentDelta); ok {
					text += v.Content
				}
				if !submitted && seen >= len(tt.before)-1 && text == "half" {
					submitted = true
					if _, err := stream.Submit(types.NewUserMessage("new direction"), SubmitInterruptReplace); err != nil {
						t.Fatalf("Submit: %v", err)
					}
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if len(provider.requests()) != 2 {
				t.Fatalf("requests = %d, want the replacement turn to run", len(provider.requests()))
			}
			if spent := budget.Spent(); spent >= types.USD(1) || spent <= 0 {
				t.Errorf("spent = %s, want a small charge for the tokens used, not the whole allowance", spent)
			}
			if budget.Uncertain() != 0 {
				t.Errorf("uncertain settlements = %d, want 0", budget.Uncertain())
			}
		})
	}
}

// TestInterruptDuringAdmissionApproval checks that an interrupt submitted
// while a budget admission approval is pending replaces the turn instead of
// failing the run with a budget error.
func TestInterruptDuringAdmissionApproval(t *testing.T) {
	provider := pricedStepProvider{newStepProvider(stepCall{
		before: append([]types.Delta{types.UsageDelta{PromptTokens: 10, CompletionTokens: 2}}, agenttest.TextResponse("done")...),
	})}
	budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), OnExceed: types.BudgetRequireApproval, ApprovalGrant: types.USD(10)})
	// The allowance is already spent, so the first call needs approval.
	if _, err := budget.Record("m", provider.Capabilities().Pricing, types.TokenUsage{InputTokens: 1_000_000, Requests: 1}); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(AgentConfig{Provider: provider}, WithBudget(budget))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("start")})
	var id SubmissionID
	markers := 0
	injected := false
	for d := range stream.Deltas() {
		switch v := d.(type) {
		case types.MarkerDelta:
			markers++
			if markers == 1 {
				var err error
				if id, err = stream.Submit(types.NewUserMessage("new direction"), SubmitInterruptReplace); err != nil {
					t.Fatalf("Submit: %v", err)
				}
				continue
			}
			stream.ResolveMarker(v.ToolCallID, true, nil)
		case types.InjectedDelta:
			injected = injected || v.SubmissionID == string(id)
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait = %v, want the replaced turn to run", err)
	}
	if !injected || len(stream.Undelivered()) != 0 {
		t.Errorf("injected = %v, undelivered = %d; want the interrupt delivered", injected, len(stream.Undelivered()))
	}
	if markers != 2 {
		t.Errorf("approval markers = %d, want 2 (one interrupted, one approved)", markers)
	}
	if reqs := provider.requests(); len(reqs) != 1 || textOf(reqs[0][len(reqs[0])-1]) != "new direction" {
		t.Errorf("requests = %v, want one call ending with the new message", reqs)
	}
}
