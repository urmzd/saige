package selector

import (
	"context"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools"
)

// The harness toolset can be disclosed lazily: the core read tools are sent
// every turn and the rest are found with tool_search.
func TestDeferredHarnessToolset(t *testing.T) {
	set, err := tools.Harness(context.Background(), tools.HarnessOptions{Root: t.TempDir(), Groups: []tools.Group{tools.GroupRead, tools.GroupWrite, tools.GroupWeb}})
	if err != nil {
		t.Fatal(err)
	}
	policy := NewDeferredTools(set.Core()...)
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", ToolSearchName, map[string]any{"query": "fetch a web page url", "k": 1}),
		agenttest.TextResponse("done"),
	}}
	a := agent.NewAgent(agent.AgentConfig{Name: "worker", Provider: provider},
		agent.WithToolset(set), agent.WithToolPolicy(policy), agent.WithTools(policy.Tool()))
	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	reqs := provider.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	first := toolNames(reqs[0].Tools)
	want := []string{"grep", "list_dir", "read_file", "scratch_read", ToolSearchName}
	if !slices.Equal(first, want) {
		t.Errorf("first turn tools = %v, want %v", first, want)
	}
	second := toolNames(reqs[1].Tools)
	if !slices.Contains(second, tools.FetchURLName) {
		t.Errorf("second turn tools = %v, want fetch_url discovered", second)
	}
}
