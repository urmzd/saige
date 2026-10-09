package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func quotaTool(name string) *agenttest.MockTool {
	return &agenttest.MockTool{Def: types.ToolDef{Name: name, Parameters: types.ParameterSchema{Type: "object"}}, Result: "ok"}
}

func TestToolQuotaAtDispatch(t *testing.T) {
	two := func(id1, id2 string) []types.Delta {
		return append(agenttest.ToolCallResponse(id1, "search", map[string]any{}),
			agenttest.ToolCallResponse(id2, "search", map[string]any{})...)
	}
	tests := []struct {
		name      string
		quota     int
		gate      types.ToolGate
		wantCalls int
		wantErrs  int
		wantUsed  int
	}{
		{name: "quota caps calls in one turn and across turns", quota: 2, wantCalls: 2, wantErrs: 1, wantUsed: 2},
		{name: "denied calls do not use the quota", quota: 1, gate: types.DenyListGate("search"), wantCalls: 0, wantErrs: 0, wantUsed: 0},
		{name: "no quota", quota: 0, wantCalls: 3, wantErrs: 0, wantUsed: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := quotaTool("search")
			budget := types.NewBudget(types.BudgetPolicy{}).ToolQuota("search", tt.quota)
			opts := []AgentOption{WithBudget(budget), WithMaxIter(5), WithSequentialTools()}
			if tt.gate != nil {
				opts = append(opts, WithToolGate(tt.gate))
			}
			a := NewAgent(AgentConfig{
				Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
					two("c1", "c2"),
					agenttest.ToolCallResponse("c3", "search", map[string]any{}),
					agenttest.TextResponse("done"),
				}},
				Tools: types.NewToolRegistry(tool),
			}, opts...)
			stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatalf("run failed: %v", err)
			}
			quotaErrs := 0
			for _, d := range deltas {
				if end, ok := d.(types.ToolExecEndDelta); ok && strings.Contains(end.Error, types.ErrToolQuotaExceeded.Error()) {
					quotaErrs++
				}
			}
			if tool.CallCount() != tt.wantCalls || quotaErrs != tt.wantErrs || budget.ToolCalls("search") != tt.wantUsed {
				t.Errorf("calls = %d, quota errors = %d, used = %d; want %d, %d, %d",
					tool.CallCount(), quotaErrs, budget.ToolCalls("search"), tt.wantCalls, tt.wantErrs, tt.wantUsed)
			}
		})
	}
}

func TestToolQuotaSharedWithParallelCalls(t *testing.T) {
	tool := quotaTool("search")
	var deltas []types.Delta
	for i := range 6 {
		deltas = append(deltas, agenttest.ToolCallResponse("c"+string(rune('a'+i)), "search", map[string]any{})...)
	}
	budget := types.NewBudget(types.BudgetPolicy{}).ToolQuota("search", 4)
	a := NewAgent(AgentConfig{
		Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{deltas, agenttest.TextResponse("done")}},
		Tools:    types.NewToolRegistry(tool),
	}, WithBudget(budget))
	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if tool.CallCount() != 4 {
		t.Fatalf("parallel calls ran %d times, quota 4", tool.CallCount())
	}
}

func TestRunScopeReachesPolicyAndTools(t *testing.T) {
	var (
		mu       sync.Mutex
		selected []RunScope
		executed []RunScope
	)
	policy := ToolPolicyFunc(func(ctx context.Context, owner string, defs []types.ToolDef) ([]string, error) {
		scope, _ := RunScopeFromContext(ctx)
		mu.Lock()
		selected = append(selected, scope)
		mu.Unlock()
		names := make([]string, len(defs))
		for i, d := range defs {
			names[i] = d.Name
		}
		return names, nil
	})
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "probe", Parameters: types.ParameterSchema{Type: "object"}},
		Fn: func(ctx context.Context, _ map[string]any) (string, error) {
			scope, _ := RunScopeFromContext(ctx)
			mu.Lock()
			executed = append(executed, scope)
			mu.Unlock()
			return "ok", nil
		},
	}
	a := NewAgent(AgentConfig{
		Name: "worker",
		Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.ToolCallResponse("c1", "probe", map[string]any{}),
			agenttest.TextResponse("done"),
		}},
		Tools: types.NewToolRegistry(tool),
	}, WithToolPolicy(policy))
	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	want := RunScope{Agent: "worker", Conversation: a.Tree().Root().ID, Branch: "main"}
	if want.Conversation == "" {
		t.Fatal("scope has no conversation")
	}
	if len(selected) != 2 || selected[0] != want || selected[1] != want {
		t.Errorf("policy scopes = %v, want %v twice", selected, want)
	}
	if len(executed) != 1 || executed[0] != want {
		t.Errorf("tool scopes = %v, want %v", executed, want)
	}
	if want.Key() == (RunScope{Agent: "worker", Conversation: want.Conversation, Branch: "other"}).Key() {
		t.Error("scope keys must differ by branch")
	}
	if want.Key() == (RunScope{Agent: "worker", Conversation: "other", Branch: "main"}).Key() {
		t.Error("scope keys must differ by conversation")
	}
}

// Two agents with the same name each start on a branch named "main", so
// only the conversation keeps their scopes apart.
func TestRunScopeDiffersPerConversation(t *testing.T) {
	var (
		mu   sync.Mutex
		keys = map[string]bool{}
	)
	policy := ToolPolicyFunc(func(ctx context.Context, _ string, defs []types.ToolDef) ([]string, error) {
		scope, _ := RunScopeFromContext(ctx)
		mu.Lock()
		keys[scope.Key()] = true
		mu.Unlock()
		return nil, nil
	})
	for range 2 {
		a := NewAgent(AgentConfig{
			Name:     "worker",
			Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}},
		}, WithToolPolicy(policy))
		stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
		agenttest.CollectDeltas(stream.Deltas())
		if err := stream.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 {
		t.Errorf("distinct scope keys = %d, want 2", len(keys))
	}
}
