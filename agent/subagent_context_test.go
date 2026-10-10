package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// userTexts returns the text blocks of the user messages in msgs.
func userTexts(msgs []types.Message) []string {
	var out []string
	for _, m := range msgs {
		if um, ok := m.(types.UserMessage); ok {
			for _, c := range um.Parts {
				if t, ok := c.(types.TextPart); ok {
					out = append(out, t.Text)
				}
			}
		}
	}
	return out
}

// runDelegation runs a parent that answers "earlier" to the first user
// message, then delegates to child on the second, and returns the child's
// first request and the delegation's tool result.
func runDelegation(t *testing.T, def SubAgentDef) (types.ToolExecEndDelta, []types.Message) {
	t.Helper()
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.TextResponse("earlier"), types.PartDeltas(1, types.ThinkingPart{Text: "private", Signature: "sig"})...),
		agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "summarize"}),
		agenttest.TextResponse("finished"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("child answer")}}
	def.Name, def.Provider = "child", child
	a := NewAgent(AgentConfig{Name: "coordinator", Provider: parent, SubAgents: []SubAgentDef{def}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("first question"))})); err != nil {
		t.Fatal(err)
	}
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("second question"))})
	var end types.ToolExecEndDelta
	for d := range stream.Deltas() {
		if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == "delegate" {
			end = e
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	var first []types.Message
	if reqs := child.Requests(); len(reqs) > 0 {
		first = reqs[0].Messages
	}
	return end, first
}

func TestSubAgentContextModes(t *testing.T) {
	tests := []struct {
		name      string
		def       SubAgentDef
		wantTexts []string // user texts the child sees, in order, caller block excluded
		wantErr   string
	}{
		{
			name:      "task only",
			def:       SubAgentDef{},
			wantTexts: []string{"summarize"},
		},
		{
			name:      "fork",
			def:       SubAgentDef{Context: ContextFork},
			wantTexts: []string{"first question", "second question", "summarize"},
		},
		{
			name: "filtered",
			def: SubAgentDef{Context: ContextFiltered, ContextFilter: MessageSelectorFunc(func(_ context.Context, h []types.Message) ([]types.Message, error) {
				return h[len(h)-1:], nil
			})},
			wantTexts: []string{"second question", "summarize"},
		},
		{
			name: "filter keeps a tool call without its result",
			def: SubAgentDef{Context: ContextFiltered, ContextFilter: MessageSelectorFunc(func(_ context.Context, h []types.Message) ([]types.Message, error) {
				call := types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "lookup-1", Name: "lookup"}}}
				return append(h, call, types.UserMsg(types.Text("next"))), nil
			})},
			wantErr: "broke tool pairing: tool call lookup-1 has no result",
		},
		{
			name: "filter keeps a tool result without its call",
			def: SubAgentDef{Context: ContextFiltered, ContextFilter: MessageSelectorFunc(func(_ context.Context, h []types.Message) ([]types.Message, error) {
				return append(h, types.ToolResults(types.ToolResultPart{CallID: "lookup-2", Parts: []types.ToolOutputPart{types.Text("x")}})), nil
			})},
			wantErr: "broke tool pairing: tool result lookup-2 has no earlier tool call",
		},
		{
			name: "filter keeps a complete tool exchange",
			def: SubAgentDef{Context: ContextFiltered, ContextFilter: MessageSelectorFunc(func(_ context.Context, h []types.Message) ([]types.Message, error) {
				call := types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "lookup-3", Name: "lookup"}}}
				return append(h[len(h)-1:], call, types.ToolResults(types.ToolResultPart{CallID: "lookup-3", Parts: []types.ToolOutputPart{types.Text("x")}}), types.UserMsg(types.Text("next"))), nil
			})},
			wantTexts: []string{"second question", "next", "summarize"},
		},
		{
			name:    "filtered without a filter",
			def:     SubAgentDef{Context: ContextFiltered},
			wantErr: "needs a ContextFilter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			end, first := runDelegation(t, tt.def)
			if tt.wantErr != "" {
				if !strings.Contains(end.Error, tt.wantErr) || first != nil {
					t.Fatalf("result = %+v, child requests = %d", end, len(first))
				}
				return
			}
			if end.Error != "" || end.Result != "child answer" {
				t.Fatalf("result = %+v", end)
			}
			var texts []string
			for _, text := range userTexts(first) {
				if !strings.HasPrefix(text, "<caller>") {
					texts = append(texts, text)
				}
			}
			if strings.Join(texts, "|") != strings.Join(tt.wantTexts, "|") {
				t.Fatalf("child saw %q, want %q", texts, tt.wantTexts)
			}
			for _, m := range first {
				am, ok := m.(types.AssistantMessage)
				if !ok {
					continue
				}
				for _, tc := range assistantToolCalls(&am) {
					if tc.ID == "delegate" {
						t.Fatal("child received the delegating turn's tool calls")
					}
				}
				for _, c := range am.Parts {
					if _, ok := c.(types.ThinkingPart); ok {
						t.Fatal("child received the parent's thinking")
					}
				}
			}
		})
	}
}

func TestCallerBlock(t *testing.T) {
	tests := []struct {
		name string
		def  SubAgentDef
		want []string
		omit bool
	}{
		{
			name: "default",
			def:  SubAgentDef{},
			want: []string{"<caller>", "coordinator > child (depth 1)", "final message is the entire result", "Do not delegate back to coordinator."},
		},
		{name: "omitted", def: SubAgentDef{OmitCallerBlock: true}, omit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, first := runDelegation(t, tt.def)
			texts := userTexts(first)
			if len(texts) == 0 {
				t.Fatal("child got no user text")
			}
			if tt.omit {
				if len(texts) != 1 || texts[0] != "summarize" {
					t.Fatalf("child saw %q", texts)
				}
				return
			}
			for _, want := range tt.want {
				if !strings.Contains(texts[0], want) {
					t.Fatalf("caller block %q lacks %q", texts[0], want)
				}
			}
			if texts[len(texts)-1] != "summarize" {
				t.Fatalf("task is not last: %q", texts)
			}
		})
	}
}

func TestDelegationToAncestorIsRefused(t *testing.T) {
	// The child is configured with a sub-agent named after its parent, so it
	// could otherwise call back up the path.
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("back", "delegate_to_coordinator", map[string]any{"task": "loop"}),
		agenttest.TextResponse("gave up"),
	}}
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "work"}),
		agenttest.TextResponse("finished"),
	}}
	a := NewAgent(AgentConfig{Name: "coordinator", Provider: parent, SubAgents: []SubAgentDef{{
		Name: "child", Provider: child,
		SubAgents: []SubAgentDef{{Name: "coordinator", Provider: &agenttest.ScriptedProvider{}}},
	}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
	var refused string
	for d := range stream.Deltas() {
		if ex, ok := d.(types.ToolExecDelta); ok {
			if e, ok := ex.Inner.(types.ToolExecEndDelta); ok && e.ToolCallID == "back" {
				refused = e.Error
			}
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refused, ErrAncestorDelegation.Error()) {
		t.Fatalf("delegation to an ancestor was not refused: %q", refused)
	}
}

func TestTextMessagesOnly(t *testing.T) {
	history := []types.Message{
		types.UserMsg(types.Text("hi")),
		types.AssistantMessage{Parts: []types.AssistantPart{types.TextPart{Text: "calling"}, types.ToolCallPart{ID: "1", Name: "x"}}},
		types.UserMessage{Parts: []types.UserPart{types.ToolResultPart{CallID: "1", Parts: []types.ToolOutputPart{types.Text("r")}}}},
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "2", Name: "x"}}},
		types.AssistantMessage{Parts: []types.AssistantPart{types.TextPart{Text: "done"}}},
	}
	got, err := TextMessagesOnly{}.SelectMessages(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	for _, m := range got {
		if am, ok := m.(types.AssistantMessage); ok && len(assistantToolCalls(&am)) > 0 {
			t.Fatal("tool call kept")
		}
	}
}

func TestChildInterruptListedAtParent(t *testing.T) {
	a := gatedChildAgent()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
	m := nextMarker(t, stream)
	if m.ToolCallID != "delegate/read" || m.Interrupt == nil {
		t.Fatalf("marker = %+v", m)
	}
	if len(m.Interrupt.Path) != 1 || m.Interrupt.Path[0] != "delegate" || m.Interrupt.RunID != stream.runID {
		t.Fatalf("child interrupt is not placed under the delegation: %+v", m.Interrupt)
	}
	pending := stream.PendingInterrupts()
	if len(pending) != 1 || pending[0].ID != m.Interrupt.ID {
		t.Fatalf("parent pending = %+v", pending)
	}
	if err := stream.ReplyInterrupt(ctx, types.InterruptReply{ID: m.Interrupt.ID, Decision: types.ApprovalDecision{Approved: true}}); err != nil {
		t.Fatal(err)
	}
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffCarriesMessageAndContext(t *testing.T) {
	entry := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("h", "handoff_to_billing", map[string]any{
			"reason": "refund", "message": "customer verified", "context": "order 42",
		}),
	}}
	billing := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("refunded")}}
	a := NewAgent(AgentConfig{Name: "triage", Provider: entry}, WithHandoffs(HandoffDef{Name: "billing", Provider: billing}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("refund please"))})); err != nil {
		t.Fatal(err)
	}
	reqs := billing.Requests()
	if len(reqs) == 0 {
		t.Fatal("billing was not called")
	}
	brief := strings.Join(userTexts(reqs[0].Messages), "\n")
	for _, want := range []string{"Task brief or return data: refund", "Handover note: customer verified", "Context: order 42"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief %q lacks %q", brief, want)
		}
	}
	msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	var stored *types.HandoffPart
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Parts {
				if h, ok := c.(types.HandoffPart); ok {
					stored = &h
				}
			}
		}
	}
	if stored == nil || stored.Message != "customer verified" || stored.Context != "order 42" {
		t.Fatalf("stored handoff = %+v", stored)
	}
}

func TestSubAgentModeStrings(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{ContextTaskOnly.String(), "task_only"},
		{ContextFork.String(), "fork"},
		{ContextFiltered.String(), "filtered"},
		{SubAgentContext(9).String(), "SubAgentContext(9)"},
		{SubAgentDelegate.String(), "delegate"},
		{SubAgentSpawn.String(), "spawn"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
