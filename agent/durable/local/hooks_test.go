package local

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// Hook and guardrail outcomes are saved with the run, so a resumed run
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
		return must.Get(agent.New(agent.Config{
			Provider: recordingInput{provider{&calls}, &seen}, SystemPrompt: "rules", MaxParallelTools: 1,
			Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), read),
		},
			agent.WithHooks(agent.Hooks{
				UserInput: func(_ context.Context, ev *agent.UserInputEvent) error {
					n := hookCalls.Add(1)
					ev.Message = types.UserMsg(types.Text(fmt.Sprintf("go (annotated %d)", n)))
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
			agent.WithOutputGuardrails(agent.OutputGuardrail{Guardrail: agent.NewGuardrail("upper", func(_ context.Context, in agent.GuardrailInput) (agent.GuardrailVerdict, error) {
				hookCalls.Add(1)
				return agent.Rewrite(strings.ToUpper(in.Text), "shout"), nil
			})}),
		))
	}
	input := []types.Message{types.UserMsg(types.Text("go"))}
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
	for _, c := range result.Parts {
		if tc, ok := c.(types.TextPart); ok {
			text += tc.Text
		}
	}
	if text != "DONE" {
		t.Errorf("final answer %q, want the recorded rewrite", text)
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

func (p recordingInput) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	m, tools := req.Messages, req.Tools
	for _, msg := range m {
		if um, ok := msg.(types.UserMessage); ok {
			for _, c := range um.Parts {
				if tc, ok := c.(types.TextPart); ok {
					p.seen.Store(tc.Text)
					return p.provider.Stream(ctx, types.Request{Messages: m, Tools: tools})
				}
			}
		}
	}
	return p.provider.Stream(ctx, types.Request{Messages: m, Tools: tools})
}
