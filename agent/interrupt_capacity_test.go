package agent

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func TestApprovalWaitDoesNotConsumeToolSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := newEventStream(ctx, cancel)
	marked := &types.ToolFunc{Def: types.ToolDef{Name: "marked"}, Fn: func(context.Context, map[string]any) (string, error) { return "ok", nil }}
	independent := make(chan struct{}, 1)
	read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: func(context.Context, map[string]any) (string, error) { independent <- struct{}{}; return "ok", nil }}
	tools := types.NewToolRegistry(types.WithMarkers(marked, types.Marker{Kind: "approval"}), read)
	a := NewAgent(AgentConfig{Provider: &mockProvider{response: "done"}, Tools: tools, MaxParallelTools: 2})
	done := make(chan struct{})
	go func() {
		a.executeToolsConcurrently(ctx, stream, []types.ToolUseContent{{ID: "1", Name: "marked"}, {ID: "2", Name: "marked"}, {ID: "3", Name: "read"}}, tools)
		close(done)
	}()
	var markers []string
	for len(markers) < 2 {
		select {
		case delta := <-stream.Deltas():
			if m, ok := delta.(types.MarkerDelta); ok {
				markers = append(markers, m.ToolCallID)
			}
		case <-ctx.Done():
			t.Fatal("markers did not arrive")
		}
	}
	select {
	case <-independent:
	case <-ctx.Done():
		t.Fatal("approvals occupied both execution slots")
	}
	for _, id := range markers {
		stream.ResolveMarker(id, true, nil)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("tools did not complete")
	}
}

func TestNonStreamingApprovalFailsWithoutResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := newEventStream(ctx, cancel)
	stream.nonStreaming = true
	a := NewAgent(AgentConfig{Provider: &mockProvider{response: "done"}})
	_, _, approved := a.awaitApproval(ctx, stream, types.ToolUseContent{ID: "call", Name: "write"}, []types.Marker{{Kind: "approval"}})
	if approved || stream.runError() == nil || ctx.Err() != nil {
		t.Fatalf("approval did not fail immediately: %v", stream.runError())
	}
}
