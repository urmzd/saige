package local

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// Hook outcomes are saved with the run, so a resumed run
// applies them as recorded even when the hooks would now decide otherwise.
func TestHookOutcomesSurviveResume(t *testing.T) {
	ctx := context.Background()
	engine := New(t.TempDir())
	var calls, hookCalls atomic.Int32
	var seen atomic.Value
	write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(_ context.Context, args map[string]any) (string, error) {
		return "wrote " + fmt.Sprint(args["value"]), nil
	}}
	read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: func(context.Context, map[string]any) (string, error) { return "read", nil }}
	factory := func() *agent.Agent {
		return agent.NewAgent(agent.AgentConfig{
			Provider: recordingInput{provider{&calls}, &seen}, SystemPrompt: "rules", MaxParallelTools: 1,
			Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), read),
		},
			agent.WithHooks(agent.Hooks{
				UserInput: func(_ context.Context, ev *agent.UserInputEvent) error {
					n := hookCalls.Add(1)
					ev.Message = types.NewUserMessage(fmt.Sprintf("go (annotated %d)", n))
					return nil
				},
				BeforeTool: func(_ context.Context, ev *agent.BeforeToolEvent) error {
					hookCalls.Add(1)
					if ev.Call.Name == "write" {
						ev.Arguments = map[string]any{"value": fmt.Sprintf("v%d", hookCalls.Load())}
					}
					return nil
				},
			}),
		)
	}
	input := []types.Message{types.NewUserMessage("go")}
	if _, err := engine.Run(ctx, "run", "v1", factory, input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	firstInput := seen.Load()
	if err := engine.Decide("run", "v1", "marker/approval-call", "key", types.ApprovalDecision{Approved: true}); err != nil {
		t.Fatal(err)
	}
	engine = New(engine.Directory)
	result, err := engine.Run(ctx, "run", "v1", factory, input)
	if err != nil || result == nil {
		t.Fatalf("%v %v", result, err)
	}
	if got := seen.Load(); got != firstInput {
		t.Errorf("resumed run sent %q, recorded run sent %q", got, firstInput)
	}
	var text string
	for _, c := range result.Content {
		if tc, ok := c.(types.TextContent); ok {
			text += tc.Text
		}
	}
	if text != "done" {
		t.Errorf("final answer %q", text)
	}
	before := hookCalls.Load()
	if _, err := engine.Run(ctx, "run", "v1", factory, input); err != nil {
		t.Fatal(err)
	}
	if hookCalls.Load() != before {
		t.Errorf("a completed run called hooks %d more times", hookCalls.Load()-before)
	}
}

// recordingInput remembers the first user text each call sends.
type recordingInput struct {
	provider
	seen *atomic.Value
}

func (p recordingInput) ChatStream(ctx context.Context, m []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	for _, msg := range m {
		if um, ok := msg.(types.UserMessage); ok {
			for _, c := range um.Content {
				if tc, ok := c.(types.TextContent); ok {
					p.seen.Store(tc.Text)
					return p.provider.ChatStream(ctx, m, tools)
				}
			}
		}
	}
	return p.provider.ChatStream(ctx, m, tools)
}
