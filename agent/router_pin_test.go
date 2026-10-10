package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/types"
)

// A model requested in the conversation must be recorded as the router
// session's pin even when the session already reports that profile.
func TestConfigModelPinsRouterSession(t *testing.T) {
	scripted := func() *agenttest.ScriptedProvider {
		return &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.TextResponse("one"), agenttest.TextResponse("two"),
		}}
	}
	r, err := router.New(router.Config{Profiles: []router.Profile{
		{ID: "a", Provider: scripted()}, {ID: "b", Provider: scripted()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	session := r.Session()
	a := NewAgent(AgentConfig{Provider: session, SystemPrompt: "sys"})
	run := func(msg types.Message) {
		t.Helper()
		stream := a.Invoke(context.Background(), []types.Message{msg})
		agenttest.CollectDeltas(stream.Deltas())
		if err := stream.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	run(types.UserMsg(types.Text("first")))
	if got := session.Model(); got != "a" {
		t.Fatalf("first turn used %q, want a", got)
	}
	run(types.UserMessage{Parts: []types.UserPart{
		types.TextPart{Text: "second"}, types.ConfigPart{Target: types.ModelTarget("a")},
	}})
	if pin := session.RouteState().Pin; pin != "a" {
		t.Fatalf("pin = %q, want a", pin)
	}
}
