package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestCollect(t *testing.T) {
	lookup := &agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}, Result: "Tokyo"}
	broken := &agenttest.MockTool{Def: types.ToolDef{Name: "broken"}, Err: errors.New("down")}
	withUsage := func(deltas []types.Delta, prompt, completion int) []types.Delta {
		return append(deltas, types.UsageDelta{PromptTokens: prompt, CompletionTokens: completion, ResponseModel: "m"})
	}
	tests := []struct {
		name       string
		responses  [][]types.Delta
		errs       []error
		wantText   string
		wantAll    string
		wantCalls  []string
		wantTurns  int
		wantErrs   int
		wantPrompt int
		wantErr    bool
	}{
		{
			name:       "single turn",
			responses:  [][]types.Delta{withUsage(agenttest.TextResponse("hello"), 3, 2)},
			wantText:   "hello",
			wantAll:    "hello",
			wantTurns:  1,
			wantPrompt: 3,
		},
		{
			name: "tool loop keeps the last turn as the answer",
			responses: [][]types.Delta{
				withUsage(append(agenttest.TextResponse("checking "), agenttest.ToolCallResponse("c1", "lookup", nil)...), 10, 1),
				withUsage(agenttest.ToolCallResponse("c2", "broken", nil), 12, 1),
				withUsage(agenttest.TextResponse("Tokyo."), 20, 3),
			},
			wantText:   "Tokyo.",
			wantAll:    "checking Tokyo.",
			wantCalls:  []string{"lookup", "broken"},
			wantTurns:  3,
			wantErrs:   1,
			wantPrompt: 42,
		},
		{
			name:      "provider error is returned",
			responses: [][]types.Delta{nil},
			errs:      []error{errors.New("boom")},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &agenttest.ScriptedProvider{Responses: tt.responses, Errors: tt.errs}
			a := must.Get(New(Config{Provider: p, Tools: types.NewToolRegistry(lookup, broken)}, WithMaxConsecutiveErrors(-1)))
			var seen int
			tr, err := Collect(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}), func(types.Delta) { seen++ })
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if seen == 0 {
				t.Error("onDelta was never called")
			}
			if tt.wantErr {
				return
			}
			if tr.Text != tt.wantText || tr.AllText != tt.wantAll {
				t.Errorf("Text = %q, AllText = %q; want %q, %q", tr.Text, tr.AllText, tt.wantText, tt.wantAll)
			}
			var names []string
			for _, c := range tr.ToolCalls {
				names = append(names, c.Name)
			}
			if len(names) != len(tt.wantCalls) {
				t.Fatalf("ToolCalls = %v, want %v", names, tt.wantCalls)
			}
			for i := range names {
				if names[i] != tt.wantCalls[i] {
					t.Fatalf("ToolCalls = %v, want %v", names, tt.wantCalls)
				}
			}
			if tr.Turns != tt.wantTurns || tr.ToolErrors != tt.wantErrs || tr.Usage.PromptTokens != tt.wantPrompt {
				t.Errorf("Turns = %d, ToolErrors = %d, PromptTokens = %d; want %d, %d, %d",
					tr.Turns, tr.ToolErrors, tr.Usage.PromptTokens, tt.wantTurns, tt.wantErrs, tt.wantPrompt)
			}
			if tr.Usage.ResponseModel != "m" {
				t.Errorf("ResponseModel = %q, want m", tr.Usage.ResponseModel)
			}
			if tr.TTFT <= 0 || tr.Total < tr.TTFT {
				t.Errorf("TTFT = %v, Total = %v", tr.TTFT, tr.Total)
			}
		})
	}
}

func TestCollectText(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("hi")}}
	a := must.Get(New(Config{Provider: p}))
	text, err := CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}))
	if err != nil || text != "hi" {
		t.Fatalf("CollectText = %q, %v", text, err)
	}
}

func TestCollectIgnoresSubAgentText(t *testing.T) {
	child := SubAgentDef{Name: "child", Description: "helps"}
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "delegate_to_child", map[string]any{"task": "t"}),
		agenttest.TextResponse("child text"),
		agenttest.TextResponse("parent text"),
	}}
	a := must.Get(New(Config{Provider: p}, WithSubAgents(child), WithSequentialTools()))
	tr, err := Collect(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.AllText != "parent text" || tr.Turns != 2 {
		t.Fatalf("AllText = %q, Turns = %d; want parent text only over 2 turns", tr.AllText, tr.Turns)
	}
}
