package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// capturingProvider records the messages it was last called with and replies
// with a fixed text response.
type capturingProvider struct {
	mu       sync.Mutex
	last     []types.Message
	response string
}

func (p *capturingProvider) ChatStream(_ context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.last = msgs
	p.mu.Unlock()
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: p.response}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

func (p *capturingProvider) snapshot() []types.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// constHandoffProvider always emits a tool call handing off to a fixed target.
type constHandoffProvider struct {
	target string
	n      int
	mu     sync.Mutex
}

func (p *constHandoffProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.n++
	id := p.target
	p.mu.Unlock()
	ch := make(chan types.Delta, 3)
	ch <- types.ToolCallStartDelta{ID: "h-" + id, Name: "handoff_to_" + p.target}
	ch <- types.ToolCallEndDelta{Arguments: map[string]any{}}
	close(ch)
	return ch, nil
}

func handoffDeltas(deltas []types.Delta) []types.HandoffDelta {
	return collectDeltasByType[types.HandoffDelta](deltas)
}

func TestSingleHandoff(t *testing.T) {
	provPlanner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("h1", "handoff_to_worker", map[string]any{}),
	}}
	provWorker := &mockProvider{response: "worker finished"}

	a := NewAgent(AgentConfig{Name: "planner", Provider: provPlanner, SystemPrompt: "root"},
		WithHandoffs(HandoffDef{Name: "worker", Provider: provWorker, SystemPrompt: "WORKER PERSONA"}))

	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("do it")})
	deltas := collectDeltas(stream)

	hds := handoffDeltas(deltas)
	if len(hds) != 1 || hds[0].From != "planner" || hds[0].To != "worker" {
		t.Fatalf("HandoffDeltas = %+v, want one planner→worker", hds)
	}
	if txt := collectText(deltas); !strings.Contains(txt, "worker finished") {
		t.Errorf("final text = %q, want it to contain worker output", txt)
	}

	// The shared branch carries a HandoffContent overlay node.
	msgs, _ := a.Tree().FlattenBranch("main")
	if !hasHandoffContent(msgs, "worker") {
		t.Error("expected a HandoffContent{To:worker} overlay node on the branch")
	}
}

func TestHandoffPreservesContext(t *testing.T) {
	provPlanner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("h1", "handoff_to_worker", map[string]any{}),
	}}
	provWorker := &capturingProvider{response: "ok"}

	a := NewAgent(AgentConfig{Name: "planner", Provider: provPlanner, SystemPrompt: "root constitution"},
		WithHandoffs(HandoffDef{Name: "worker", Provider: provWorker, SystemPrompt: "WORKER PERSONA"}))

	collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("remember XYZ")}))

	got := provWorker.snapshot()
	if !messagesContainText(got, "remember XYZ") {
		t.Error("worker did not receive the full prior conversation context")
	}
	// The worker persona overlays the root system message (not a 2nd system msg).
	if len(got) == 0 {
		t.Fatal("worker received no messages")
	}
	sm, ok := got[0].(types.SystemMessage)
	if !ok {
		t.Fatalf("first message is %T, want SystemMessage", got[0])
	}
	if !systemContains(sm, "root constitution") || !systemContains(sm, "WORKER PERSONA") {
		t.Errorf("root system message should carry both shared root and worker persona, got %+v", sm)
	}
}

func TestHandBack(t *testing.T) {
	provPlanner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("h1", "handoff_to_worker", map[string]any{}),
		agenttest.TextResponse("planner final"),
	}}
	provWorker := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("h2", "handoff_to_planner", map[string]any{}),
	}}

	a := NewAgent(AgentConfig{Name: "planner", Provider: provPlanner, SystemPrompt: "root"},
		WithHandoffs(HandoffDef{Name: "worker", Provider: provWorker, SystemPrompt: "worker"}))

	deltas := collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}))

	hds := handoffDeltas(deltas)
	if len(hds) != 2 {
		t.Fatalf("HandoffDeltas = %d, want 2 (planner→worker→planner)", len(hds))
	}
	if hds[0].To != "worker" || hds[1].To != "planner" {
		t.Errorf("handoff sequence = %v→%v, want worker then planner", hds[0].To, hds[1].To)
	}
	if txt := collectText(deltas); !strings.Contains(txt, "planner final") {
		t.Errorf("final text = %q, want 'planner final'", txt)
	}
}

func TestPingPongBounded(t *testing.T) {
	provPlanner := &constHandoffProvider{target: "worker"}
	provWorker := &constHandoffProvider{target: "planner"}

	a := NewAgent(AgentConfig{Name: "planner", Provider: provPlanner, SystemPrompt: "root"},
		WithHandoffs(HandoffDef{Name: "worker", Provider: provWorker, SystemPrompt: "worker"}),
		WithMaxHandoffs(2))

	deltas := collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}))

	errs := collectDeltasByType[types.ErrorDelta](deltas)
	if len(errs) == 0 || !errors.Is(errs[0].Error, ErrHandoffLimitExceeded) {
		t.Fatalf("expected ErrHandoffLimitExceeded, got %+v", errs)
	}
	// MaxHandoffs=2 means exactly 2 transfers happen, then the next is rejected
	// before being streamed or persisted.
	if hds := handoffDeltas(deltas); len(hds) != 2 {
		t.Errorf("handoffs = %d, want exactly 2 (== MaxHandoffs)", len(hds))
	}
}

func TestHumanForcedHandoff(t *testing.T) {
	// Planner must never run: the human forces control straight to the worker.
	a := NewAgent(AgentConfig{Name: "planner", Provider: panicProvider{}, SystemPrompt: "root"},
		WithHandoffs(HandoffDef{Name: "worker", Provider: &mockProvider{response: "worker reply"}, SystemPrompt: "worker"}))

	input := []types.Message{types.UserMessage{Content: []types.UserContent{
		types.HandoffContent{To: "worker"},
		types.TextContent{Text: "hello"},
	}}}
	deltas := collectDeltas(a.Invoke(context.Background(), input))

	if txt := collectText(deltas); !strings.Contains(txt, "worker reply") {
		t.Errorf("final text = %q, want worker reply (human-forced handoff)", txt)
	}
}

func TestHandoffValidationPanicsOnUnknownTarget(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected NewAgent to panic on an unknown handoff target")
		}
	}()
	NewAgent(AgentConfig{Name: "planner", Provider: &mockProvider{}, Handoffs: []HandoffDef{
		{Name: "worker", Provider: &mockProvider{}, CanHandOffTo: []string{"ghost"}},
	}})
}

func TestHandoffSerializeRoundTrip(t *testing.T) {
	provPlanner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("h1", "handoff_to_worker", map[string]any{}),
	}}
	a := NewAgent(AgentConfig{Name: "planner", Provider: provPlanner, SystemPrompt: "root"},
		WithHandoffs(HandoffDef{Name: "worker", Provider: &mockProvider{response: "ok"}, SystemPrompt: "worker"}))
	collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}))

	raw, err := json.Marshal(a.Tree())
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := tree.New(types.NewSystemMessage("placeholder"))
	if err := json.Unmarshal(raw, restored); err != nil {
		t.Fatalf("unmarshal tree: %v", err)
	}
	msgs, err := restored.FlattenBranch("main")
	if err != nil {
		t.Fatal(err)
	}
	if !hasHandoffContent(msgs, "worker") {
		t.Error("HandoffContent did not survive tree serialization round-trip")
	}
}

// ── helpers ─────────────────────────────────────────────────────────

func hasHandoffContent(msgs []types.Message, to string) bool {
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Content {
				if hc, ok := c.(types.HandoffContent); ok && hc.To == to {
					return true
				}
			}
		}
	}
	return false
}

func messagesContainText(msgs []types.Message, substr string) bool {
	for _, m := range msgs {
		switch v := m.(type) {
		case types.UserMessage:
			for _, c := range v.Content {
				if tc, ok := c.(types.TextContent); ok && strings.Contains(tc.Text, substr) {
					return true
				}
			}
		case types.SystemMessage:
			if systemContains(v, substr) {
				return true
			}
		}
	}
	return false
}

func systemContains(sm types.SystemMessage, substr string) bool {
	for _, c := range sm.Content {
		if tc, ok := c.(types.TextContent); ok && strings.Contains(tc.Text, substr) {
			return true
		}
	}
	return false
}

func collectText(deltas []types.Delta) string {
	var s strings.Builder
	for _, d := range deltas {
		if tc, ok := d.(types.TextContentDelta); ok {
			s.WriteString(tc.Content)
		}
	}
	return s.String()
}
