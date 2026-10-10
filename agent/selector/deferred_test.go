package selector

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func mockTool(name, description string) *agenttest.MockTool {
	return &agenttest.MockTool{
		Def: types.ToolDef{
			Name:        name,
			Description: description,
			Parameters:  types.ParameterSchema{Type: "object"},
		},
		Result: name + " ok",
	}
}

func toolNames(defs []types.ToolDef) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	slices.Sort(names)
	return names
}

// runDeferred runs one conversation of an agent named "worker" and returns
// the provider, the deltas, and the run's scope.
func runDeferred(t *testing.T, policy *DeferredTools, responses [][]types.Delta, tools ...types.Tool) (*agenttest.ScriptedProvider, []types.Delta, agent.RunScope) {
	t.Helper()
	provider := &agenttest.ScriptedProvider{Responses: responses}
	a := must.Get(agent.New(agent.Config{
		Name:     "worker",
		Provider: provider,
		Tools:    types.NewToolRegistry(tools...),
	}, agent.WithToolPolicy(policy), agent.WithMaxIter(6)))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
	deltas := agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return provider, deltas, agent.RunScope{Agent: "worker", Conversation: a.Tree().Root().ID, Branch: "main"}
}

func toolEnd(deltas []types.Delta, id string) types.ToolExecEndDelta {
	for _, d := range deltas {
		if end, ok := d.(types.ToolExecEndDelta); ok && end.ToolCallID == id {
			return end
		}
	}
	return types.ToolExecEndDelta{}
}

func TestDeferredToolsDiscoversThroughSearch(t *testing.T) {
	policy := NewDeferredTools("read_file")
	weather := mockTool("get_weather", "Returns the weather forecast for a city")
	provider, deltas, scope := runDeferred(t, policy, [][]types.Delta{
		agenttest.ToolCallResponse("c1", ToolSearchName, map[string]any{"query": "weather forecast"}),
		agenttest.ToolCallResponse("c2", "get_weather", map[string]any{}),
		agenttest.TextResponse("sunny"),
	},
		policy.Tool(),
		mockTool("read_file", "Read a file"),
		mockTool("send_email", "Send an email"),
		weather,
	)

	reqs := provider.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	tests := []struct {
		turn int
		want []string
	}{
		{0, []string{"read_file", ToolSearchName}},
		{1, []string{"get_weather", "read_file", ToolSearchName}},
		{2, []string{"get_weather", "read_file", ToolSearchName}},
	}
	for _, tt := range tests {
		if got := toolNames(reqs[tt.turn].Tools); !slices.Equal(got, tt.want) {
			t.Errorf("turn %d tools = %v, want %v", tt.turn, got, tt.want)
		}
	}

	var result struct {
		Tools []struct {
			Name       string `json:"name"`
			Capability string `json:"capability"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolEnd(deltas, "c1").Result), &result); err != nil {
		t.Fatalf("tool_search result is not JSON: %v", err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "get_weather" || result.Tools[0].Capability != "" {
		t.Errorf("tool_search result = %+v, want get_weather without capability", result.Tools)
	}
	if weather.CallCount() != 1 {
		t.Errorf("get_weather calls = %d, want 1", weather.CallCount())
	}
	if got := policy.Discovered(scope); !slices.Equal(got, []string{"get_weather"}) {
		t.Errorf("Discovered = %v", got)
	}
}

// Two agents with the same name share one policy, as a server that builds an
// agent per request does. Discovery in the first conversation must not show
// up in the second.
func TestDeferredToolsDiscoveryStaysInItsConversation(t *testing.T) {
	policy := NewDeferredTools("read_file")
	tools := func() []types.Tool {
		return []types.Tool{policy.Tool(), mockTool("read_file", "Read a file"), mockTool("get_weather", "Returns the weather forecast for a city")}
	}
	_, _, first := runDeferred(t, policy, [][]types.Delta{
		agenttest.ToolCallResponse("c1", ToolSearchName, map[string]any{"query": "weather forecast"}),
		agenttest.TextResponse("found"),
	}, tools()...)
	provider, _, second := runDeferred(t, policy, [][]types.Delta{
		agenttest.TextResponse("hello"),
	}, tools()...)

	if first.Key() == second.Key() {
		t.Fatal("two conversations share a scope key")
	}
	tests := []struct {
		name  string
		scope agent.RunScope
		want  []string
	}{
		{"first conversation", first, []string{"get_weather"}},
		{"second conversation", second, nil},
	}
	for _, tt := range tests {
		if got := policy.Discovered(tt.scope); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Discovered = %v, want %v", tt.name, got, tt.want)
		}
	}
	if got, want := toolNames(provider.Requests()[0].Tools), []string{"read_file", ToolSearchName}; !slices.Equal(got, want) {
		t.Errorf("second conversation tools = %v, want %v", got, want)
	}
}

func TestDeferredToolsHiddenToolIsNotExecutable(t *testing.T) {
	policy := NewDeferredTools()
	email := mockTool("send_email", "Send an email")
	_, deltas, _ := runDeferred(t, policy, [][]types.Delta{
		agenttest.ToolCallResponse("c1", "send_email", map[string]any{}),
		agenttest.TextResponse("done"),
	}, policy.Tool(), email)
	if email.CallCount() != 0 {
		t.Fatal("a hidden tool must not run before it is discovered")
	}
	if end := toolEnd(deltas, "c1"); !strings.Contains(end.Error, "tool not found") {
		t.Errorf("error = %q, want tool not found", end.Error)
	}
}

func TestDeferredToolsSelect(t *testing.T) {
	defs := []types.ToolDef{{Name: "a"}, {Name: "b"}, {Name: ToolSearchName}}
	tests := []struct {
		name   string
		pinned []string
		defs   []types.ToolDef
		want   []string
	}{
		{"pinned and search only", []string{"b"}, defs, []string{"b", ToolSearchName}},
		{"nothing pinned", nil, defs, []string{ToolSearchName}},
		{"without the search tool nothing is hidden", nil, defs[:2], []string{"a", "b"}},
		{"pinned name not registered is skipped", []string{"zzz"}, defs, []string{ToolSearchName}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewDeferredTools(tt.pinned...).Select(context.Background(), "x", tt.defs)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Select = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeferredToolsScopesAreSeparate(t *testing.T) {
	policy := NewDeferredTools()
	defs := []types.ToolDef{{Name: "alpha_tool", Description: "alpha"}, {Name: ToolSearchName}}
	one := agent.WithRunScope(context.Background(), agent.RunScope{Agent: "w", Branch: "b1"})
	two := agent.WithRunScope(context.Background(), agent.RunScope{Agent: "w", Branch: "b2"})
	for _, ctx := range []context.Context{one, two} {
		if _, err := policy.Select(ctx, "w", defs); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := policy.Tool().Execute(one, map[string]any{"query": "alpha"}); err != nil {
		t.Fatal(err)
	}
	got1, _ := policy.Select(one, "w", defs)
	got2, _ := policy.Select(two, "w", defs)
	if !slices.Contains(got1, "alpha_tool") || slices.Contains(got2, "alpha_tool") {
		t.Errorf("discovery leaked across scopes: b1=%v b2=%v", got1, got2)
	}
	policy.Forget(agent.RunScope{Agent: "w", Branch: "b1"})
	if got, _ := policy.Select(one, "w", defs); slices.Contains(got, "alpha_tool") {
		t.Errorf("Forget kept discovery: %v", got)
	}
}

func TestDeferredToolsSearchOutsideAgent(t *testing.T) {
	if _, err := NewDeferredTools().Tool().Execute(context.Background(), map[string]any{"query": "x"}); err == nil {
		t.Fatal("tool_search with no selected tool list must fail")
	}
}

func TestDeferredToolsBoundsScopes(t *testing.T) {
	policy := &DeferredTools{MaxScopes: 2}
	defs := []types.ToolDef{{Name: ToolSearchName}}
	for _, b := range []types.BranchID{"1", "2", "3"} {
		ctx := agent.WithRunScope(context.Background(), agent.RunScope{Agent: "w", Branch: b})
		if _, err := policy.Select(ctx, "w", defs); err != nil {
			t.Fatal(err)
		}
	}
	if len(policy.scopes) != 2 {
		t.Fatalf("scopes = %d, want 2", len(policy.scopes))
	}
	if _, ok := policy.scopes[agent.RunScope{Agent: "w", Branch: "1"}.Key()]; ok {
		t.Error("the oldest scope should be evicted first")
	}
}

func TestDeferredToolsConcurrentSearch(t *testing.T) {
	policy := &DeferredTools{DefaultResults: 1}
	defs := []types.ToolDef{{Name: ToolSearchName}}
	for i := range 10 {
		defs = append(defs, types.ToolDef{Name: "tool_" + string(rune('a'+i)), Description: "shared"})
	}
	ctx := agent.WithRunScope(context.Background(), agent.RunScope{Agent: "w"})
	if _, err := policy.Select(ctx, "w", defs); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = policy.Tool().Execute(ctx, map[string]any{"query": "shared", "k": float64(3)})
		}()
		go func() {
			defer wg.Done()
			_, _ = policy.Select(ctx, "w", defs)
		}()
	}
	wg.Wait()
	if got := policy.Discovered(agent.RunScope{Agent: "w"}); len(got) == 0 {
		t.Error("concurrent searches discovered nothing")
	}
}
