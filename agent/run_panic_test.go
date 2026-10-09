package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// panicTracer panics when a run starts.
type panicTracer struct{}

func (panicTracer) StartAgent(context.Context, string) (context.Context, func(error)) {
	panic("tracer broke")
}

// TestRunPanicEndsRunWithError checks that a panic in caller-supplied code
// on the run's own goroutine ends that run with an error, closes its stream,
// and releases the branch, instead of crashing the process.
func TestRunPanicEndsRunWithError(t *testing.T) {
	tests := []struct {
		name      string
		panicAt   int32 // ToolPolicy call that panics; 0 never
		tracer    bool
		wantErr   string
		wantCalls int // provider calls before the run ended
	}{
		{name: "tool policy panics on the first turn", panicAt: 1, wantErr: "policy broke", wantCalls: 0},
		{name: "tool policy panics after a tool turn", panicAt: 2, wantErr: "policy broke", wantCalls: 1},
		{name: "tracer panic does not end the run", tracer: true, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "echo"}, Result: "ok"}
			provider := newStepProvider(
				stepCall{before: agenttest.ToolCallResponse("c1", "echo", map[string]any{})},
				stepCall{before: agenttest.TextResponse("done")},
			)
			var calls atomic.Int32
			opts := []AgentOption{WithToolPolicy(ToolPolicyFunc(func(_ context.Context, _ string, defs []types.ToolDef) ([]string, error) {
				if calls.Add(1) == tt.panicAt {
					panic("policy broke")
				}
				names := make([]string, len(defs))
				for i, d := range defs {
					names[i] = d.Name
				}
				return names, nil
			}))}
			cfg := AgentConfig{Provider: provider, Tools: types.NewToolRegistry(tool)}
			if tt.tracer {
				cfg.RunTracer = panicTracer{}
			}
			a := NewAgent(cfg, opts...)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("go")})
			for range stream.Deltas() {
			}
			err := stream.Wait()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Wait = %v, want success", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Wait = %v, want an error naming %q", err, tt.wantErr)
			}
			if got := len(provider.requests()); got != tt.wantCalls {
				t.Errorf("provider calls = %d, want %d", got, tt.wantCalls)
			}
			// The branch claim was released: another run can start.
			next := a.Invoke(ctx, []types.Message{types.NewUserMessage("again")})
			for range next.Deltas() {
			}
			if err := next.Wait(); err != nil && strings.Contains(err.Error(), "active") {
				t.Fatalf("branch still claimed: %v", err)
			}
		})
	}
}

// TestAnswerOpenToolCalls checks that the calls of a trailing assistant turn
// get error results, and that a branch without open calls is left alone.
func TestAnswerOpenToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		tail        []types.Message
		wantResults int
	}{
		{
			name:        "trailing tool calls are answered",
			tail:        []types.Message{types.AssistantMessage{Content: []types.AssistantContent{types.ToolUseContent{ID: "c1", Name: "echo"}, types.ToolUseContent{ID: "c2", Name: "echo"}}}},
			wantResults: 2,
		},
		{
			name: "text answer is left alone",
			tail: []types.Message{types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "hi"}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(AgentConfig{Provider: newStepProvider()})
			ctx := context.Background()
			branch := a.Tree().Active()
			for _, m := range append([]types.Message{types.NewUserMessage("go")}, tt.tail...) {
				if err := a.appendToBranch(ctx, a.Tree(), branch, m); err != nil {
					t.Fatal(err)
				}
			}
			a.answerOpenToolCalls(ctx, branch, "agent run panicked")
			msgs, err := a.Tree().FlattenBranch(branch)
			if err != nil {
				t.Fatal(err)
			}
			var results []types.ToolResultContent
			if sm, ok := msgs[len(msgs)-1].(types.SystemMessage); ok {
				for _, c := range sm.Content {
					if r, ok := c.(types.ToolResultContent); ok {
						results = append(results, r)
					}
				}
			}
			if len(results) != tt.wantResults {
				t.Fatalf("tool results = %+v, want %d", results, tt.wantResults)
			}
			for _, r := range results {
				if !r.IsError || r.Text != "agent run panicked" {
					t.Errorf("result = %+v, want an error result", r)
				}
			}
		})
	}
}
