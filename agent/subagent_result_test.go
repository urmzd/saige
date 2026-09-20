package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

type resultSinkFunc func(context.Context, SubAgentResult) error

func (f resultSinkFunc) Save(ctx context.Context, r SubAgentResult) error { return f(ctx, r) }

func TestSubAgentResultFinalAndTraceIsolation(t *testing.T) {
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.TextResponse("working"), agenttest.ToolCallResponse("read", "read", nil)...),
		agenttest.TextResponse(`{"answer":42}`),
	}}
	registry := types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "read"}, Result: "data"})
	a := NewAgent(AgentConfig{Name: "parent", SubAgents: []SubAgentDef{{Name: "child", Provider: child, Tools: registry}}})
	stream, err := a.InvokeSubAgent(context.Background(), "child", "task")
	if err != nil {
		t.Fatal(err)
	}
	agenttest.CollectDeltas(stream.Deltas())
	result, err := stream.SubAgentResult()
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != `{"answer":42}` {
		t.Fatalf("output = %q", result.Output)
	}
	if result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) {
		t.Fatal("missing timestamps")
	}
	messages, err := result.Messages()
	if err != nil || len(messages) != 5 {
		t.Fatalf("messages = %v, %v", messages, err)
	}
	if !strings.Contains(string(result.Trace), "working") {
		t.Fatal("intermediate message missing")
	}
	tr, err := result.Tree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = result.Node(tr.Root().ID); err != nil {
		t.Fatal(err)
	}
	result.Trace[0] = '!'
	second, err := stream.SubAgentResult()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = second.Tree(); err != nil {
		t.Fatal("reader mutated retained result", err)
	}
	if len(registry.All()) != 1 {
		t.Fatal("caller registry changed")
	}
}

func TestSubAgentResultFailuresAreNotSuccess(t *testing.T) {
	for _, tc := range []struct {
		name                string
		runError, saveError bool
	}{
		{"provider", true, false}, {"sink", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := agenttest.TextResponse("done")
			if tc.runError {
				response = append(response, types.ErrorDelta{Error: errors.New("failed")})
			}
			var saved SubAgentResult
			a := NewAgent(AgentConfig{SubAgents: []SubAgentDef{{Name: "child", Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{response}}, ResultSink: resultSinkFunc(func(_ context.Context, r SubAgentResult) error {
				saved = r
				if tc.saveError {
					return errors.New("storage unavailable")
				}
				return nil
			})}}})
			stream, _ := a.InvokeSubAgent(context.Background(), "child", "task")
			agenttest.CollectDeltas(stream.Deltas())
			result, err := stream.SubAgentResult()
			if err == nil || result.Error == "" || result.Output != "" || len(saved.Trace) == 0 {
				t.Fatalf("result %+v, %v", result, err)
			}
			if _, err = result.FinalAssistant(); err == nil {
				t.Fatal("failed result accepted as final")
			}
		})
	}
}

func TestSubAgentApprovalCanResolveImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "read"}), agenttest.TextResponse("finished")}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("read", "read", nil), agenttest.TextResponse("child done")}}
	gate := types.GateFunc(func(_ context.Context, def types.ToolDef, _ map[string]any) types.GateDecision {
		if def.Name == "read" {
			return types.GateDecision{Outcome: types.GateRequireApproval}
		}
		return types.Allow()
	})
	a := NewAgent(AgentConfig{Provider: parent, ToolGate: gate, SubAgents: []SubAgentDef{{Name: "child", Provider: child, Tools: types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "read"}, Result: "ok"})}}})
	stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("go")})
	approved := false
	for delta := range stream.Deltas() {
		if marker, ok := delta.(types.MarkerDelta); ok {
			if marker.ToolCallID != "delegate/read" {
				t.Fatalf("unscoped marker %q", marker.ToolCallID)
			}
			stream.ResolveMarker(marker.ToolCallID, true, nil)
			approved = true
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if !approved {
		t.Fatal("no approval")
	}
}

func TestHandoffOwnerContextResumesWithoutOtherTranscript(t *testing.T) {
	ctx := HandoffContext{Entry: "triage", Target: "triage", Messages: []types.Message{
		types.NewSystemMessage("root"), types.NewUserMessage("task"), types.NewAssistantMessage("triage work"),
		types.SystemMessage{Content: []types.SystemContent{types.HandoffContent{From: "triage", To: "specialist", Reason: "inspect data"}}},
		types.NewAssistantMessage("private specialist work"),
		types.SystemMessage{Content: []types.SystemContent{types.HandoffContent{From: "specialist", To: "triage", Reason: "cannot answer: missing account"}}},
	}}
	messages, err := (OwnerContext{}).Select(context.Background(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resultMessagesContain(messages, "private specialist work") || !resultMessagesContain(messages, "triage work") || !resultMessagesContain(messages, "missing account") {
		t.Fatalf("incorrect owner context: %+v", messages)
	}
	ctx.Target = "specialist"
	messages, _ = (OwnerContext{}).Select(context.Background(), ctx)
	if resultMessagesContain(messages, "triage work") || !resultMessagesContain(messages, "task") {
		t.Fatal("specialist received caller transcript or lost task")
	}
}

func TestHandoffLinkPolicies(t *testing.T) {
	links := map[string][]string{"triage": {"specialist"}, "specialist": {"other"}, "other": {}}
	got, _ := (DirectReturnLinks{}).Resolve(links)
	if len(got["specialist"]) != 2 || got["specialist"][1] != "triage" {
		t.Fatalf("missing return link: %v", got)
	}
	strict, _ := (DirectedLinks{}).Resolve(links)
	if len(strict["specialist"]) != 1 || len(links["specialist"]) != 1 {
		t.Fatal("graph mutated")
	}
}

func TestResultPolicySelectsStructuredData(t *testing.T) {
	a := NewAgent(AgentConfig{SubAgents: []SubAgentDef{{
		Name: "child", Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("raw")}},
		ResultPolicy: SubAgentResultFunc(func(result SubAgentResult) (string, error) {
			messages, err := result.Messages()
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(map[string]any{"messages": len(messages), "id": result.ID})
			return string(data), err
		}),
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
	var selected struct {
		Messages int
		ID       string
	}
	if err = json.Unmarshal([]byte(result.Output), &selected); err != nil || selected.Messages != 3 || selected.ID != result.ID {
		t.Fatal("selection failed", err)
	}
}

func TestToolPolicyRejectsHiddenCalls(t *testing.T) {
	called := false
	tool := &types.ToolFunc{Def: types.ToolDef{Name: "hidden"}, Fn: func(context.Context, map[string]any) (string, error) { called = true; return "unexpected", nil }}
	model := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("call", "hidden", nil), agenttest.TextResponse("done")}}
	a := NewAgent(AgentConfig{Provider: model, Tools: types.NewToolRegistry(tool)}, WithToolPolicy(ToolPolicyFunc(func(context.Context, string, []types.ToolDef) ([]string, error) { return nil, nil })))
	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("task")})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unadvertised tool executed")
	}
}

func resultMessagesContain(messages []types.Message, text string) bool {
	raw, _ := json.Marshal(messages)
	return strings.Contains(string(raw), text)
}
