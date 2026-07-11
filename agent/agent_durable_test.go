package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func assistantText(m *types.AssistantMessage) string {
	if m == nil {
		return ""
	}
	var s string
	for _, c := range m.Content {
		if t, ok := c.(types.TextContent); ok {
			s += t.Text
		}
	}
	return s
}

func TestNoopStepRunnerPassThrough(t *testing.T) {
	ran := 0
	res, err := types.NoopStepRunner{}.RunStep(context.Background(), "x", func(context.Context) (types.StepResult, error) {
		ran++
		return types.StepResult{Kind: types.StepKindTool, ToolResult: "v"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("fn ran %d times, want 1", ran)
	}
	if res.ToolResult != "v" {
		t.Errorf("result = %q, want v", res.ToolResult)
	}
}

func TestRunDurableFirstRunRunsSteps(t *testing.T) {
	runner := newRecordingRunner()
	a := NewAgent(AgentConfig{Provider: &mockProvider{response: "live"}, SystemPrompt: "s"})

	final, err := a.RunDurable(context.Background(), runner, []types.Message{types.NewUserMessage("hi")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if assistantText(final) != "live" {
		t.Errorf("final = %q, want live", assistantText(final))
	}
	if !runner.has("llm-main-0") {
		t.Error("expected step llm-main-0 to be recorded")
	}
	if runner.ranCount("llm-main-0") != 1 {
		t.Errorf("llm-main-0 ran %d times, want 1", runner.ranCount("llm-main-0"))
	}
}

func TestRunDurableReplaySkipsProvider(t *testing.T) {
	canned := types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "memoized"}}}
	runner := newRecordingRunner()
	runner.seed("llm-main-0", types.StepResult{Kind: types.StepKindLLM, Message: &canned})

	// panicProvider would panic if the LLM step were re-executed.
	a := NewAgent(AgentConfig{Provider: panicProvider{}, SystemPrompt: "s"})

	final, err := a.RunDurable(context.Background(), runner, []types.Message{types.NewUserMessage("hi")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if assistantText(final) != "memoized" {
		t.Errorf("final = %q, want memoized (from record)", assistantText(final))
	}
}

func TestRunDurableToolMemoization(t *testing.T) {
	prov := &toolCallProvider{toolName: "act", toolID: "call-1", toolArgs: map[string]any{"k": "v"}, response: "done"}
	tool := &countTool{name: "act"}
	runner := newRecordingRunner()
	// Pretend the tool step already completed before a crash.
	runner.seed("tool-call-1", types.StepResult{Kind: types.StepKindTool, ToolCallID: "call-1", ToolResult: "memoized tool output"})

	a := NewAgent(AgentConfig{
		Provider:     prov,
		Tools:        types.NewToolRegistry(tool),
		SystemPrompt: "s",
	})

	final, err := a.RunDurable(context.Background(), runner, []types.Message{types.NewUserMessage("go")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tool.count() != 0 {
		t.Errorf("tool executed %d times, want 0 (memoized)", tool.count())
	}
	if assistantText(final) != "done" {
		t.Errorf("final = %q, want done", assistantText(final))
	}

	// The memoized tool output must appear in the persisted tool-result message.
	msgs, _ := a.Tree().FlattenBranch("main")
	found := false
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Content {
				if tr, ok := c.(types.ToolResultContent); ok && tr.Text == "memoized tool output" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected the memoized tool output in the tree's tool-result message")
	}
}

func TestRunDurableDistinctStepNames(t *testing.T) {
	prov := &toolCallProvider{toolName: "act", toolID: "call-1", toolArgs: map[string]any{}, response: "done"}
	tool := &countTool{name: "act"}
	runner := newRecordingRunner()

	a := NewAgent(AgentConfig{Provider: prov, Tools: types.NewToolRegistry(tool), SystemPrompt: "s"})
	if _, err := a.RunDurable(context.Background(), runner, []types.Message{types.NewUserMessage("go")}, ""); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"llm-main-0", "tool-call-1", "llm-main-1"} {
		if runner.ranCount(name) != 1 {
			t.Errorf("step %q ran %d times, want 1 (unique, stable names)", name, runner.ranCount(name))
		}
	}
}

// A cancelled durable run must return the terminal error, never a stale
// success: the in-band ErrorDelta can be dropped once the context is done, so
// RunDurable relies on the stream's close error.
func TestRunDurableCancelledReturnsError(t *testing.T) {
	blocking := &blockingProvider{started: make(chan struct{})}
	ag := NewAgent(AgentConfig{
		Name:         "durable-cancel",
		SystemPrompt: "sys",
		Provider:     blocking,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-blocking.started
		cancel()
	}()

	msg, err := ag.RunDurable(ctx, nil, []types.Message{types.NewUserMessage("hi")}, "")
	if err == nil {
		t.Fatalf("RunDurable after cancellation = (%v, nil), want error", msg)
	}
}

// blockingProvider signals when called, then blocks until the context dies.
type blockingProvider struct {
	started chan struct{}
}

func (p *blockingProvider) ChatStream(ctx context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	close(p.started)
	ch := make(chan types.Delta)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
