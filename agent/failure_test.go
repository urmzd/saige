package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// ===================================================================
// Wait() error propagation
// ===================================================================

// A run that fails must report the failure BOTH as an ErrorDelta and as the
// stream's close error, so Wait()-only consumers cannot miss it.
func TestStreamWaitReturnsProviderError(t *testing.T) {
	provider := &errorProvider{err: errors.New("connection refused")}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	deltas := collectDeltas(stream)

	err := stream.Wait()
	if err == nil {
		t.Fatal("Wait() = nil, want provider error")
	}
	if !errors.Is(err, types.ErrProviderFailed) {
		t.Errorf("Wait() = %v, want ErrProviderFailed", err)
	}

	errorDeltas := collectDeltasByType[types.ErrorDelta](deltas)
	if len(errorDeltas) != 1 {
		t.Fatalf("ErrorDelta count = %d, want 1", len(errorDeltas))
	}
	if !errors.Is(err, errorDeltas[0].Error) {
		t.Errorf("Wait() error %v does not match ErrorDelta %v", err, errorDeltas[0].Error)
	}
}

func TestStreamWaitReturnsMaxIterationsError(t *testing.T) {
	provider := &multiTurnToolProvider{toolTurns: 100, toolName: "noop"}
	registry := types.NewToolRegistry()
	registry.Register(&staticTool{name: "noop", result: "ok"})

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        registry,
		MaxIter:      2,
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	collectDeltas(stream)

	if err := stream.Wait(); !errors.Is(err, types.ErrMaxIterations) {
		t.Errorf("Wait() = %v, want ErrMaxIterations", err)
	}
}

func TestStreamWaitNilOnCleanFinish(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "done"},
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	collectDeltas(stream)

	if err := stream.Wait(); err != nil {
		t.Errorf("Wait() = %v, want nil", err)
	}
}

// staticTool returns a fixed result; used to keep the loop iterating.
type staticTool struct {
	name   string
	result string
}

func (t *staticTool) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }
func (t *staticTool) Execute(context.Context, map[string]any) (string, error) {
	return t.result, nil
}

// ===================================================================
// Sub-agent failure propagation
// ===================================================================

// A child agent whose provider fails must produce a FAILED parent tool result,
// not a success with partial text.
func TestSubAgentFailureFailsParentToolResult(t *testing.T) {
	childProvider := &errorProvider{err: errors.New("child provider down")}
	parentProvider := &toolCallProvider{
		toolName: "delegate_to_helper",
		toolID:   "call-1",
		toolArgs: map[string]any{"task": "do something"},
		response: "parent done",
	}

	agent := NewAgent(AgentConfig{
		Provider:     parentProvider,
		SystemPrompt: "parent sys",
		SubAgents: []SubAgentDef{
			{Name: "helper", Description: "helper", SystemPrompt: "child sys", Provider: childProvider},
		},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("delegate")})
	deltas := collectDeltas(stream)
	stream.Wait()

	// The delegation's ToolExecEndDelta must carry the child's error.
	var end *types.ToolExecEndDelta
	for _, d := range collectDeltasByType[types.ToolExecEndDelta](deltas) {
		if d.ToolCallID == "call-1" {
			e := d
			end = &e
		}
	}
	if end == nil {
		t.Fatal("no ToolExecEndDelta for delegation call-1")
	}
	if end.Error == "" {
		t.Error("ToolExecEndDelta.Error is empty, want child provider error")
	}
	if !strings.Contains(end.Error, "child provider down") {
		t.Errorf("ToolExecEndDelta.Error = %q, want it to contain the child error", end.Error)
	}

	// The persisted tool result must be marked IsError.
	msgs, err := agent.Tree().FlattenBranch("main")
	if err != nil {
		t.Fatalf("FlattenBranch: %v", err)
	}
	found := false
	for _, m := range msgs {
		sm, ok := m.(types.SystemMessage)
		if !ok {
			continue
		}
		for _, c := range sm.Content {
			if trc, ok := c.(types.ToolResultContent); ok && trc.ToolCallID == "call-1" {
				found = true
				if !trc.IsError {
					t.Error("persisted tool result IsError = false, want true")
				}
			}
		}
	}
	if !found {
		t.Error("no persisted tool result for call-1")
	}
}

// ===================================================================
// Sub-agent StepRunner inheritance
// ===================================================================

// A delegated child agent must run its steps through the parent's StepRunner
// (namespaced by the delegating tool call) so durable execution covers child
// work too.
func TestSubAgentInheritsStepRunner(t *testing.T) {
	runner := newRecordingRunner()

	childProvider := &mockProvider{response: "child result"}
	parentProvider := &toolCallProvider{
		toolName: "delegate_to_helper",
		toolID:   "call-1",
		toolArgs: map[string]any{"task": "do something"},
		response: "parent done",
	}

	agent := NewAgent(AgentConfig{
		Provider:     parentProvider,
		SystemPrompt: "parent sys",
		StepRunner:   runner,
		SubAgents: []SubAgentDef{
			{Name: "helper", Description: "helper", SystemPrompt: "child sys", Provider: childProvider},
		},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("delegate")})
	collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Parent LLM steps run under their own names.
	if !runner.has("llm-main-0") {
		t.Error("parent LLM step llm-main-0 not recorded")
	}
	// The child's LLM step runs through the SAME runner, namespaced under the
	// delegating tool call so it cannot collide with the parent's step names.
	if !runner.has("sub-call-1-llm-main-0") {
		t.Error("child LLM step sub-call-1-llm-main-0 not recorded; child did not inherit the parent's StepRunner")
	}
}

// A child invoked outside any durable run (NoopStepRunner parent) keeps inline
// execution: nothing is recorded because there is no runner to thread.
func TestSubAgentNoopRunnerNotWrapped(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "hi"},
		SystemPrompt: "sys",
	})
	if r := agent.childStepRunner("call-1"); r != nil {
		t.Errorf("childStepRunner under NoopStepRunner = %T, want nil", r)
	}
}
