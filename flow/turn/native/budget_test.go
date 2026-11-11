package native_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	types "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

func TestTurnBudgetPreventsAnotherProviderRequest(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.ToolCallResponse("c1", "unavailable", map[string]any{}), types.UsageDelta{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}), agenttest.TextResponse("must not run"),
	}}
	r, err := native.New(offlineRevision(1, "Help."), native.WithProvider(native.ProviderOffline, func(native.Spec) (types.Provider, error) { return p, nil }))
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(context.Background(), native.Turn{Text: "help", Budget: &turn.Budget{Requests: 1}}, nil)
	if err != nil || result.Stopped == nil {
		t.Fatalf("request ceiling: %+v %v", result, err)
	}
	if len(p.Calls) != 1 {
		t.Fatalf("made %d provider calls past cap", len(p.Calls))
	}
}
