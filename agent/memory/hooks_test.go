package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestExtractionHookStoresMemories(t *testing.T) {
	store := NewMemStore()
	policy := Policy{Scope: func(_ context.Context, owner string) (Scope, error) {
		if owner != "helper" {
			return Scope{}, errors.New("unknown agent")
		}
		return tenant, nil
	}}
	var seen int
	ex := ExtractorFunc(func(_ context.Context, msgs []types.Message) ([]Record, error) {
		seen = len(msgs)
		// A scope the extractor names is ignored; the policy decides.
		return []Record{{Kind: KindSemantic, Content: "prefers metric units", Scope: other}}, nil
	})
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("noted")}}
	a := agent.NewAgent(agent.AgentConfig{Name: "helper", SystemPrompt: "s", Provider: p},
		agent.WithHooks(ExtractionHook(store, policy, ex)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := agent.Collect(a.Invoke(ctx, []types.Message{types.NewUserMessage("use metric")}), nil); err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Errorf("extractor saw %d messages, want the whole branch", seen)
	}
	recs, err := store.Recall(context.Background(), tenant, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Content != "prefers metric units" || recs[0].Source.Agent != "helper" {
		t.Fatalf("stored %+v", recs)
	}
	if others, _ := store.Recall(context.Background(), other, "", 0); len(others) != 0 {
		t.Errorf("the extractor's scope was used: %+v", others)
	}
}

func TestExtractionHookSkipsFailedRuns(t *testing.T) {
	called := false
	hooks := ExtractionHook(NewMemStore(), Policy{Scope: func(context.Context, string) (Scope, error) { return tenant, nil }},
		ExtractorFunc(func(context.Context, []types.Message) ([]Record, error) { called = true; return nil, nil }))
	if err := hooks.RunStop(context.Background(), &agent.RunStopEvent{Reason: agent.RunStopFailed}); err != nil || called {
		t.Fatalf("err %v called %v", err, called)
	}
}
