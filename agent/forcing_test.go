package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// plainProvider hides the ScriptedProvider's options method, standing in for a
// provider that cannot receive request options.
type plainProvider struct{ p *agenttest.ScriptedProvider }

func (p plainProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	m, t := req.Messages, req.Tools
	return p.p.Stream(ctx, types.Request{Messages: m, Tools: t})
}

func runAgent(t *testing.T, a *Agent, input ...types.Message) ([]types.Delta, error) {
	t.Helper()
	if len(input) == 0 {
		input = []types.Message{types.UserMsg(types.Text("go"))}
	}
	stream := a.Invoke(context.Background(), input)
	deltas := agenttest.CollectDeltas(stream.Deltas())
	return deltas, stream.Wait()
}

func toolCalls(n int, name string, args map[string]any) [][]types.Delta {
	out := make([][]types.Delta, n)
	for i := range out {
		out[i] = agenttest.ToolCallResponse("call-"+string(rune('a'+i)), name, args)
	}
	return out
}

func TestOnMaxIter(t *testing.T) {
	tests := []struct {
		name     string
		policy   MaxIterPolicy
		final    []types.Delta
		wantErr  error
		wantText string
	}{
		{name: "error is the default", policy: MaxIterError, wantErr: types.ErrMaxIterations},
		{name: "force final", policy: MaxIterForceFinal, final: agenttest.TextResponse("best answer"), wantText: "best answer"},
		{
			name: "force final drops a tool call the model makes anyway", policy: MaxIterForceFinal,
			final:    append(agenttest.TextResponse("answer"), agenttest.ToolCallResponse("x", "step", nil)...),
			wantText: "answer",
		},
		{name: "force final with no reply keeps the limit error", policy: MaxIterForceFinal, final: nil, wantErr: types.ErrMaxIterations},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := append(toolCalls(2, "step", map[string]any{"n": 1}), tt.final)
			// Distinct arguments so the repeat guard stays out of the way.
			responses[1] = agenttest.ToolCallResponse("call-b", "step", map[string]any{"n": 2})
			script := &agenttest.ScriptedProvider{Responses: responses}
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "step"}, Result: "ok"}
			a := NewAgent(AgentConfig{
				Provider: script, SystemPrompt: "sys", Tools: types.NewToolRegistry(tool),
				MaxIter: 2, OnMaxIter: tt.policy,
			})
			_, err := runAgent(t, a)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.policy != MaxIterForceFinal {
				if script.CallCount() != 2 {
					t.Fatalf("calls = %d, want 2", script.CallCount())
				}
				return
			}
			calls := script.Requests()
			if len(calls) != 3 {
				t.Fatalf("calls = %d, want 3", len(calls))
			}
			last := calls[2]
			if len(last.Tools) != 0 {
				t.Fatal("the forced final call must not offer tools to a provider without tool choice")
			}
			prompt := types.MessagesToText(last.Messages[len(last.Messages)-1:])
			if prompt != "User: "+DefaultForceFinalPrompt+"\n" {
				t.Fatalf("final prompt = %q", prompt)
			}
			if tool.CallCount() != 2 {
				t.Fatalf("tool ran %d times, want 2", tool.CallCount())
			}
			msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
			for _, m := range msgs {
				if types.MessagesToText([]types.Message{m}) == prompt {
					t.Fatal("the forced final prompt must not be stored in the tree")
				}
			}
			if tt.wantText == "" {
				return
			}
			tip := msgs[len(msgs)-1].(types.AssistantMessage)
			if len(assistantToolCalls(&tip)) != 0 || types.MessagesToText(msgs[len(msgs)-1:]) != "Assistant: "+tt.wantText+"\n" {
				t.Fatalf("final turn = %+v", tip)
			}
		})
	}
}

func TestStopAtTools(t *testing.T) {
	tests := []struct {
		name      string
		toolErr   error
		wantStop  bool
		wantCalls int
	}{
		{name: "success stops the run", wantStop: true, wantCalls: 1},
		{name: "failure lets the model recover", toolErr: errors.New("boom"), wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "finish", map[string]any{}),
				agenttest.TextResponse("after"),
			}}
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "finish"}, Result: "final output", Err: tt.toolErr}
			a := NewAgent(AgentConfig{Provider: script, SystemPrompt: "sys", Tools: types.NewToolRegistry(tool)},
				WithStopAtTools("finish"))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if script.CallCount() != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", script.CallCount(), tt.wantCalls)
			}
			if got := stream.StopToolCallID() == "c1"; got != tt.wantStop {
				t.Fatalf("StopToolCallID = %q", stream.StopToolCallID())
			}
		})
	}
}

func TestStopAtToolsSubAgentOutput(t *testing.T) {
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "submit", map[string]any{}),
	}}
	registry := types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "submit"}, Result: `{"id":7}`})
	a := NewAgent(AgentConfig{Name: "parent", SubAgents: []SubAgentDef{{
		Name: "child", Provider: child, Tools: registry, Options: []AgentOption{WithStopAtTools("submit")},
	}}})
	stream, err := a.InvokeSubAgent(context.Background(), "child", "task")
	if err != nil {
		t.Fatal(err)
	}
	agenttest.CollectDeltas(stream.Deltas())
	result, err := stream.SubAgentResult()
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != `{"id":7}` || result.StopToolCallID != "c1" {
		t.Fatalf("result = %q, stop = %q", result.Output, result.StopToolCallID)
	}
}

func TestMaxConsecutiveErrors(t *testing.T) {
	failing := errors.New("broken")
	tests := []struct {
		name      string
		limit     int
		policy    MaxIterPolicy
		turns     int
		recoverAt int // turn whose tool call succeeds; -1 for none
		wantErr   error
		wantCalls int
	}{
		{name: "default stops after two failed turns", limit: 0, turns: 5, recoverAt: -1, wantErr: ErrToolErrorLimit, wantCalls: 2},
		{name: "custom limit", limit: 3, turns: 5, recoverAt: -1, wantErr: ErrToolErrorLimit, wantCalls: 3},
		{name: "a success resets the count", limit: 2, turns: 3, recoverAt: 1, wantCalls: 4},
		{name: "negative disables the check", limit: -1, turns: 3, recoverAt: -1, wantCalls: 4},
		{name: "force final answers instead", limit: 2, policy: MaxIterForceFinal, turns: 2, recoverAt: -1, wantCalls: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var responses [][]types.Delta
			for i := range tt.turns {
				name := "bad"
				if i == tt.recoverAt {
					name = "good"
				}
				responses = append(responses, agenttest.ToolCallResponse("c"+string(rune('a'+i)), name, map[string]any{"i": i}))
			}
			responses = append(responses, agenttest.TextResponse("done"))
			script := &agenttest.ScriptedProvider{Responses: responses}
			tools := types.NewToolRegistry(
				&agenttest.MockTool{Def: types.ToolDef{Name: "bad"}, Err: failing},
				&agenttest.MockTool{Def: types.ToolDef{Name: "good"}, Result: "ok"},
			)
			a := NewAgent(AgentConfig{
				Provider: script, SystemPrompt: "sys", Tools: tools, MaxIter: 10,
				MaxConsecutiveErrors: tt.limit, OnMaxIter: tt.policy,
			})
			_, err := runAgent(t, a)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if script.CallCount() != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", script.CallCount(), tt.wantCalls)
			}
		})
	}
}

func TestRefusedCallsAreNotToolFaults(t *testing.T) {
	failing := errors.New("broken")
	gate := types.GateFunc(func(_ context.Context, def types.ToolDef, _ map[string]any) types.GateDecision {
		switch def.Name {
		case "denied":
			return types.GateDecision{Outcome: types.GateDeny, Reason: "not allowed"}
		case "ask":
			return types.GateDecision{Outcome: types.GateRequireApproval, Reason: "confirm"}
		}
		return types.GateDecision{Outcome: types.GateAllow}
	})
	tests := []struct {
		name      string
		turns     []string // tool called on each turn
		wantErr   error
		wantCalls int
	}{
		{name: "denied by the gate", turns: []string{"denied", "denied", "denied"}, wantCalls: 4},
		{name: "rejected by a human", turns: []string{"ask", "ask", "ask"}, wantCalls: 4},
		{name: "a refusal neither adds nor resets", turns: []string{"bad", "denied", "bad", "bad"}, wantErr: ErrToolErrorLimit, wantCalls: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var responses [][]types.Delta
			for i, name := range tt.turns {
				responses = append(responses, agenttest.ToolCallResponse("c"+string(rune('a'+i)), name, map[string]any{"i": i}))
			}
			responses = append(responses, agenttest.TextResponse("done"))
			script := &agenttest.ScriptedProvider{Responses: responses}
			tools := types.NewToolRegistry(
				&agenttest.MockTool{Def: types.ToolDef{Name: "bad"}, Err: failing},
				&agenttest.MockTool{Def: types.ToolDef{Name: "denied"}, Result: "ok"},
				&agenttest.MockTool{Def: types.ToolDef{Name: "ask"}, Result: "ok"},
			)
			a := NewAgent(AgentConfig{
				Provider: script, SystemPrompt: "sys", Tools: tools, MaxIter: 10, ToolGate: gate,
			})
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			for d := range stream.Deltas() {
				if m, ok := d.(types.MarkerDelta); ok {
					stream.ResolveMarkerWithMessage(m.ToolCallID, false, nil, "no")
				}
			}
			err := stream.Wait()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if script.CallCount() != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", script.CallCount(), tt.wantCalls)
			}
		})
	}
}

func TestMaxRepeatIterations(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantErr   error
		wantRuns  int
		wantCalls int
	}{
		{name: "disabled by default", limit: 0, wantRuns: 3, wantCalls: 4},
		{name: "third identical turn is not run", limit: 2, wantErr: ErrRepeatedToolCalls, wantRuns: 2, wantCalls: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := append(toolCalls(3, "poll", map[string]any{"q": "same"}), agenttest.TextResponse("done"))
			script := &agenttest.ScriptedProvider{Responses: responses}
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "poll"}, Result: "pending"}
			a := NewAgent(AgentConfig{
				Provider: script, SystemPrompt: "sys", Tools: types.NewToolRegistry(tool),
				MaxRepeatIterations: tt.limit,
			})
			_, err := runAgent(t, a)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tool.CallCount() != tt.wantRuns {
				t.Fatalf("tool ran %d times, want %d", tool.CallCount(), tt.wantRuns)
			}
			if script.CallCount() != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", script.CallCount(), tt.wantCalls)
			}
			// Skipped calls are still answered, so the branch stays valid.
			if err := checkToolPairing(a.Tree(), a.Tree().Active()); err != nil {
				t.Fatal(err)
			}
			msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
			if _, ok := msgs[len(msgs)-1].(types.SystemMessage); tt.wantErr != nil && !ok {
				t.Fatal("the skipped calls must have results")
			}
		})
	}
}

func TestToolChoice(t *testing.T) {
	named := types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}
	required := types.ToolChoice{Mode: types.ToolChoiceRequired}
	none := types.ToolChoice{Mode: types.ToolChoiceNone}
	tests := []struct {
		name        string
		configured  *types.ToolChoice
		inline      *types.ToolChoice // sent as ConfigPart with the input
		plain       bool              // provider without request options
		wantErr     error
		wantOptions []*types.ToolChoice // per call; nil means no options sent
		wantTools   []bool              // per call: were tools offered
	}{
		{
			name: "configured named applies to the first turn only", configured: &named,
			wantOptions: []*types.ToolChoice{&named, nil}, wantTools: []bool{true, true},
		},
		{
			name: "inline required applies to one turn", inline: &required,
			wantOptions: []*types.ToolChoice{&required, nil}, wantTools: []bool{true, true},
		},
		{
			name: "none without declared support withholds tools on every turn", configured: &none,
			wantOptions: []*types.ToolChoice{nil}, wantTools: []bool{false},
		},
		{name: "forced choice needs request options", configured: &named, plain: true, wantErr: types.ErrInvalidModelConfig},
		{
			name: "named tool must be offered", configured: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"},
			wantErr: types.ErrInvalidModelConfig,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "lookup", map[string]any{}),
				agenttest.TextResponse("done"),
			}}
			if len(tt.wantTools) == 1 {
				script.Responses = [][]types.Delta{agenttest.TextResponse("done")}
			}
			var provider types.Provider = script
			if tt.plain {
				provider = plainProvider{script}
			}
			a := NewAgent(AgentConfig{
				Provider: provider, SystemPrompt: "sys", ToolChoice: tt.configured,
				Tools: types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}, Result: "r"}),
			})
			input := types.UserMessage{Parts: []types.UserPart{types.TextPart{Text: "go"}}}
			if tt.inline != nil {
				input.Parts = append(input.Parts, types.ConfigPart{ToolChoice: tt.inline})
			}
			_, err := runAgent(t, a, input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if script.CallCount() != 0 {
					t.Fatal("an invalid tool choice must be rejected before the request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := script.Requests()
			if len(calls) != len(tt.wantOptions) {
				t.Fatalf("calls = %d, want %d", len(calls), len(tt.wantOptions))
			}
			for i, call := range calls {
				var got *types.ToolChoice
				if call.Options != nil {
					got = call.Options.ToolChoice
				}
				if (got == nil) != (tt.wantOptions[i] == nil) || (got != nil && *got != *tt.wantOptions[i]) {
					t.Fatalf("call %d tool choice = %+v, want %+v", i, got, tt.wantOptions[i])
				}
				if (len(call.Tools) > 0) != tt.wantTools[i] {
					t.Fatalf("call %d offered tools = %v, want %v", i, len(call.Tools) > 0, tt.wantTools[i])
				}
			}
		})
	}
}

func TestToolChoiceNoneWithDeclaredSupport(t *testing.T) {
	script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}}
	provider := toolChoiceScripted{script}
	tools, opts, err := toolChoiceRequest(provider, &types.ToolChoice{Mode: types.ToolChoiceNone},
		[]types.ToolDef{{Name: "lookup"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || opts == nil || opts.ToolChoice.Mode != types.ToolChoiceNone {
		t.Fatalf("tools = %v, opts = %+v; a provider that declares tool choice keeps its tools", tools, opts)
	}
}

type toolChoiceScripted struct{ *agenttest.ScriptedProvider }

func (toolChoiceScripted) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "scripted", Model: "m", Known: true,
		Caps: map[types.Capability]bool{types.CapStreaming: true, types.CapTools: true, types.CapToolChoice: true}}
}
