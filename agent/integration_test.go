package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/store/memwal"
	"github.com/urmzd/saige/agent/tree"
)

// ===================================================================
// Mock Providers
// ===================================================================

// mockProvider returns a fixed text response.
type mockProvider struct {
	response string
}

func (m *mockProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: m.response}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

// mockTokenizer implements types.Tokenizer for testing.
type mockTokenizer struct {
	tokensPerMessage int
}

func (m *mockTokenizer) CountTokens(_ context.Context, messages []types.Message) (int, error) {
	return len(messages) * m.tokensPerMessage, nil
}

// toolCallProvider emits a tool call on the first invocation and text on subsequent ones.
type toolCallProvider struct {
	mu       sync.Mutex
	calls    int
	toolName string
	toolID   string
	toolArgs map[string]any
	response string
}

func (p *toolCallProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan types.Delta, 10)
	if call == 0 {
		ch <- types.ToolCallStartDelta{ID: p.toolID, Name: p.toolName}
		ch <- types.ToolCallArgumentDelta{Content: `{"key":"value"}`}
		ch <- types.ToolCallEndDelta{Arguments: p.toolArgs}
	} else {
		ch <- types.TextStartDelta{}
		ch <- types.TextContentDelta{Content: p.response}
		ch <- types.TextEndDelta{}
	}
	close(ch)
	return ch, nil
}

// multiToolCallProvider emits multiple tool calls in one response.
type multiToolCallProvider struct {
	mu        sync.Mutex
	calls     int
	toolCalls []struct {
		ID   string
		Name string
		Args map[string]any
	}
	response string
}

func (p *multiToolCallProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan types.Delta, 20)
	if call == 0 {
		for _, tc := range p.toolCalls {
			ch <- types.ToolCallStartDelta{ID: tc.ID, Name: tc.Name}
			ch <- types.ToolCallEndDelta{Arguments: tc.Args}
		}
	} else {
		ch <- types.TextStartDelta{}
		ch <- types.TextContentDelta{Content: p.response}
		ch <- types.TextEndDelta{}
	}
	close(ch)
	return ch, nil
}

// multiTurnToolProvider calls a tool for the first N invocations, then responds with text.
type multiTurnToolProvider struct {
	mu           sync.Mutex
	calls        int
	toolTurns    int
	toolName     string
	finalMessage string
}

func (p *multiTurnToolProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan types.Delta, 10)
	if call < p.toolTurns {
		id := fmt.Sprintf("call-%d", call)
		ch <- types.ToolCallStartDelta{ID: id, Name: p.toolName}
		ch <- types.ToolCallEndDelta{Arguments: map[string]any{"step": float64(call)}}
	} else {
		ch <- types.TextStartDelta{}
		ch <- types.TextContentDelta{Content: p.finalMessage}
		ch <- types.TextEndDelta{}
	}
	close(ch)
	return ch, nil
}

// errorProvider always returns an error from ChatStream.
type errorProvider struct {
	err error
}

func (p *errorProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	return nil, &types.ProviderError{
		Provider: "error-mock",
		Kind:     types.ErrorKindPermanent,
		Err:      p.err,
	}
}

// emptyProvider returns an empty channel (no deltas).
type emptyProvider struct{}

func (p *emptyProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

// recordingProvider records messages sent to it and responds with text.
type recordingProvider struct {
	mu       sync.Mutex
	calls    [][]types.Message
	response string
}

func (p *recordingProvider) ChatStream(_ context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	copied := make([]types.Message, len(msgs))
	copy(copied, msgs)
	p.calls = append(p.calls, copied)
	p.mu.Unlock()

	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: p.response}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

// delayedProvider waits for a signal before responding. Used for cancellation tests.
type delayedProvider struct {
	ready    chan struct{}
	response string
}

func (p *delayedProvider) ChatStream(ctx context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 10)
	go func() {
		defer close(ch)
		select {
		case <-p.ready:
			ch <- types.TextStartDelta{}
			ch <- types.TextContentDelta{Content: p.response}
			ch <- types.TextEndDelta{}
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// sequenceProvider returns a specific sequence of responses based on call index.
type sequenceProvider struct {
	mu        sync.Mutex
	calls     int
	responses []func(ch chan<- types.Delta)
}

func (p *sequenceProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan types.Delta, 20)
	if idx < len(p.responses) {
		p.responses[idx](ch)
	}
	close(ch)
	return ch, nil
}

// ===================================================================
// Helper: collect all deltas from a stream
// ===================================================================

func collectDeltas(stream *EventStream) []types.Delta {
	var deltas []types.Delta
	for d := range stream.Deltas() {
		deltas = append(deltas, d)
	}
	return deltas
}

func collectDeltasByType[T types.Delta](deltas []types.Delta) []T {
	var result []T
	for _, d := range deltas {
		if v, ok := d.(T); ok {
			result = append(result, v)
		}
	}
	return result
}

func textFromDeltas(deltas []types.Delta) string {
	var sb strings.Builder
	for _, d := range deltas {
		if tc, ok := d.(types.TextContentDelta); ok {
			sb.WriteString(tc.Content)
		}
	}
	return sb.String()
}

// ===================================================================
// Agent Core Loop
// ===================================================================

func TestAgentTextOnlyResponse(t *testing.T) {
	provider := &mockProvider{response: "Hello, world!"}
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "You are a helper.",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	deltas := collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should contain TextStart, TextContent, TextEnd, Done
	starts := collectDeltasByType[types.TextStartDelta](deltas)
	contents := collectDeltasByType[types.TextContentDelta](deltas)
	ends := collectDeltasByType[types.TextEndDelta](deltas)
	dones := collectDeltasByType[types.DoneDelta](deltas)

	if len(starts) != 1 {
		t.Errorf("TextStartDelta count = %d, want 1", len(starts))
	}
	if len(contents) != 1 || contents[0].Content != "Hello, world!" {
		t.Errorf("TextContentDelta = %v, want 'Hello, world!'", contents)
	}
	if len(ends) != 1 {
		t.Errorf("TextEndDelta count = %d, want 1", len(ends))
	}
	if len(dones) != 1 {
		t.Errorf("DoneDelta count = %d, want 1", len(dones))
	}
}

func TestAgentSingleToolCall(t *testing.T) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "greet", Description: "greet"},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			return "tool result: greeted", nil
		},
	}

	provider := &toolCallProvider{
		toolName: "greet",
		toolID:   "call-1",
		toolArgs: map[string]any{"name": "test"},
		response: "Done!",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("greet me")})
	deltas := collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should see tool exec start/end deltas
	execStarts := collectDeltasByType[types.ToolExecStartDelta](deltas)
	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)

	if len(execStarts) != 1 {
		t.Errorf("ToolExecStartDelta count = %d, want 1", len(execStarts))
	}
	if execStarts[0].Name != "greet" {
		t.Errorf("tool name = %s, want greet", execStarts[0].Name)
	}
	if len(execEnds) != 1 {
		t.Errorf("ToolExecEndDelta count = %d, want 1", len(execEnds))
	}
	if execEnds[0].Result != "tool result: greeted" {
		t.Errorf("tool result = %q, want 'tool result: greeted'", execEnds[0].Result)
	}

	// Final text response
	text := textFromDeltas(deltas)
	if !strings.Contains(text, "Done!") {
		t.Errorf("expected final text 'Done!', got %q", text)
	}

	// Verify tree has all messages persisted: system, user, assistant(tool call), tool result, assistant(text)
	msgs, _ := agent.Tree().FlattenBranch("main")
	if len(msgs) != 5 {
		t.Fatalf("tree messages = %d, want 5", len(msgs))
	}
	if msgs[0].Role() != types.RoleSystem {
		t.Error("msgs[0] not system")
	}
	if msgs[1].Role() != types.RoleUser {
		t.Error("msgs[1] not user")
	}
	if msgs[2].Role() != types.RoleAssistant {
		t.Error("msgs[2] not assistant (tool call)")
	}
	if msgs[3].Role() != types.RoleSystem {
		t.Error("msgs[3] not system (tool result)")
	}
	if msgs[4].Role() != types.RoleAssistant {
		t.Error("msgs[4] not assistant (final)")
	}
}

func TestAgentMultipleToolCallsInParallel(t *testing.T) {
	var mu sync.Mutex
	execOrder := []string{}

	toolA := &types.ToolFunc{
		Def: types.ToolDef{Name: "tool_a", Description: "a"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			mu.Lock()
			execOrder = append(execOrder, "a")
			mu.Unlock()
			return "result-a", nil
		},
	}
	toolB := &types.ToolFunc{
		Def: types.ToolDef{Name: "tool_b", Description: "b"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			mu.Lock()
			execOrder = append(execOrder, "b")
			mu.Unlock()
			return "result-b", nil
		},
	}

	provider := &multiToolCallProvider{
		toolCalls: []struct {
			ID   string
			Name string
			Args map[string]any
		}{
			{ID: "call-a", Name: "tool_a", Args: map[string]any{}},
			{ID: "call-b", Name: "tool_b", Args: map[string]any{}},
		},
		response: "Both done.",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(toolA, toolB),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("do both")})
	deltas := collectDeltas(stream)
	stream.Wait()

	// Both tools should have been executed
	mu.Lock()
	if len(execOrder) != 2 {
		t.Errorf("exec order length = %d, want 2", len(execOrder))
	}
	mu.Unlock()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 2 {
		t.Errorf("ToolExecEndDelta count = %d, want 2", len(execEnds))
	}

	// Verify tool results are in the tree as a single SystemMessage
	msgs, _ := agent.Tree().FlattenBranch("main")
	// system + user + assistant(2 tool calls) + system(2 tool results) + assistant(text)
	if len(msgs) != 5 {
		t.Fatalf("tree messages = %d, want 5", len(msgs))
	}
	// The tool result message should contain both results
	sysMsg, ok := msgs[3].(types.SystemMessage)
	if !ok {
		t.Fatal("msgs[3] not SystemMessage")
	}
	if len(sysMsg.Content) != 2 {
		t.Errorf("tool result content blocks = %d, want 2", len(sysMsg.Content))
	}
}

func TestAgentToolNotFound(t *testing.T) {
	provider := &toolCallProvider{
		toolName: "nonexistent_tool",
		toolID:   "call-1",
		toolArgs: map[string]any{},
		response: "After error",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("call it")})
	deltas := collectDeltas(stream)
	stream.Wait()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 1 {
		t.Fatalf("ToolExecEndDelta count = %d, want 1", len(execEnds))
	}
	if !strings.Contains(execEnds[0].Error, "tool not found") {
		t.Errorf("expected 'tool not found' error, got %q", execEnds[0].Error)
	}

	// The error should be persisted as a tool result with IsError flag
	msgs, _ := agent.Tree().FlattenBranch("main")
	sysMsg := msgs[3].(types.SystemMessage)
	tr := sysMsg.Content[0].(types.ToolResultContent)
	if !tr.IsError {
		t.Error("expected IsError to be true for tool-not-found result")
	}
	if !strings.Contains(tr.Text, "tool not found") {
		t.Errorf("tool result text = %q, expected 'tool not found' message", tr.Text)
	}
}

func TestAgentToolReturnsError(t *testing.T) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "failing", Description: "always fails"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			return "", errors.New("tool broke")
		},
	}

	provider := &toolCallProvider{
		toolName: "failing",
		toolID:   "call-1",
		toolArgs: map[string]any{},
		response: "After failure",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("do it")})
	deltas := collectDeltas(stream)
	stream.Wait()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 1 {
		t.Fatalf("ToolExecEndDelta count = %d, want 1", len(execEnds))
	}
	if execEnds[0].Error != "tool broke" {
		t.Errorf("tool error = %q, want 'tool broke'", execEnds[0].Error)
	}
	if execEnds[0].Result != "" {
		t.Errorf("tool result = %q, want empty string on error", execEnds[0].Result)
	}
}

func TestAgentMultiTurnToolLoop(t *testing.T) {
	callCount := 0
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "step_tool", Description: "step"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			callCount++
			return fmt.Sprintf("step-%d", callCount), nil
		},
	}

	provider := &multiTurnToolProvider{
		toolTurns:    3,
		toolName:     "step_tool",
		finalMessage: "All steps done",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("multi-step")})
	deltas := collectDeltas(stream)
	stream.Wait()

	if callCount != 3 {
		t.Errorf("tool was called %d times, want 3", callCount)
	}

	text := textFromDeltas(deltas)
	if text != "All steps done" {
		t.Errorf("final text = %q, want 'All steps done'", text)
	}

	// Tree: system + user + 3*(assistant+toolresult) + final_assistant = 2 + 6 + 1 = 9
	msgs, _ := agent.Tree().FlattenBranch("main")
	if len(msgs) != 9 {
		t.Errorf("tree messages = %d, want 9", len(msgs))
	}
}

func TestAgentMaxIterationsEnforced(t *testing.T) {
	// Provider always wants to call a tool -- never sends text-only.
	infiniteToolProvider := &sequenceProvider{
		responses: make([]func(ch chan<- types.Delta), 100),
	}
	for i := range infiniteToolProvider.responses {
		infiniteToolProvider.responses[i] = func(ch chan<- types.Delta) {
			id := fmt.Sprintf("call-%d", i)
			ch <- types.ToolCallStartDelta{ID: id, Name: "repeat"}
			ch <- types.ToolCallEndDelta{Arguments: map[string]any{}}
		}
	}

	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "repeat", Description: "repeat"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			return "ok", nil
		},
	}

	agent := NewAgent(AgentConfig{
		Provider:     infiniteToolProvider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
		MaxIter:      3,
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("loop")})
	collectDeltas(stream)
	stream.Wait()

	// With MaxIter=3, should have at most 3 tool call rounds
	infiniteToolProvider.mu.Lock()
	calls := infiniteToolProvider.calls
	infiniteToolProvider.mu.Unlock()

	if calls > 3 {
		t.Errorf("provider called %d times, expected at most 3", calls)
	}
}

func TestAgentDefaultMaxIter(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "hi"},
		SystemPrompt: "sys",
	})
	// Default MaxIter is 10
	if agent.cfg.MaxIter != 10 {
		t.Errorf("default MaxIter = %d, want 10", agent.cfg.MaxIter)
	}
}

// ===================================================================
// Provider Error Handling
// ===================================================================

func TestAgentProviderError(t *testing.T) {
	provider := &errorProvider{err: errors.New("connection refused")}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	deltas := collectDeltas(stream)
	stream.Wait()

	errorDeltas := collectDeltasByType[types.ErrorDelta](deltas)
	if len(errorDeltas) != 1 {
		t.Fatalf("ErrorDelta count = %d, want 1", len(errorDeltas))
	}
	if !errors.Is(errorDeltas[0].Error, types.ErrProviderFailed) {
		t.Errorf("expected ErrProviderFailed, got %v", errorDeltas[0].Error)
	}
}

func TestAgentEmptyProviderResponse(t *testing.T) {
	provider := &emptyProvider{}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	deltas := collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Empty response => aggregator returns nil => loop breaks => DoneDelta
	dones := collectDeltasByType[types.DoneDelta](deltas)
	if len(dones) != 1 {
		t.Errorf("DoneDelta count = %d, want 1", len(dones))
	}

	// Tree should only have system + user (no assistant)
	msgs, _ := agent.Tree().FlattenBranch("main")
	if len(msgs) != 2 {
		t.Errorf("tree messages = %d, want 2", len(msgs))
	}
}

// ===================================================================
// Stream Cancellation
// ===================================================================

func TestAgentCancellation(t *testing.T) {
	provider := &delayedProvider{
		ready:    make(chan struct{}),
		response: "never",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})

	// Cancel the stream immediately
	stream.Cancel()

	// Should complete quickly
	done := make(chan struct{})
	go func() {
		stream.Wait()
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not complete after cancellation")
	}
}

func TestAgentContextCancellation(t *testing.T) {
	provider := &delayedProvider{
		ready:    make(chan struct{}),
		response: "never",
	}

	ctx, cancel := context.WithCancel(context.Background())
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(ctx, []types.Message{types.NewUserMessage("Hi")})

	// Cancel via context
	cancel()

	done := make(chan struct{})
	go func() {
		stream.Wait()
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not complete after context cancellation")
	}
}

func TestStreamCancelIdempotent(t *testing.T) {
	provider := &mockProvider{response: "hi"}
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	collectDeltas(stream)
	stream.Wait()

	// Calling Cancel multiple times should not panic
	stream.Cancel()
	stream.Cancel()
	stream.Cancel()
}

// ===================================================================
// Sub-Agent Integration
// ===================================================================

func TestSubAgentDelegation(t *testing.T) {
	childProvider := &mockProvider{response: "child result"}

	// Parent provider calls delegate_to_helper on first call, then returns text.
	parentProvider := &toolCallProvider{
		toolName: "delegate_to_helper",
		toolID:   "call-1",
		toolArgs: map[string]any{"task": "do something"},
		response: "Parent done based on child.",
	}

	agent := NewAgent(AgentConfig{
		Provider:     parentProvider,
		SystemPrompt: "parent sys",
		SubAgents: []SubAgentDef{
			{
				Name:         "helper",
				Description:  "A helper agent",
				SystemPrompt: "child sys",
				Provider:     childProvider,
			},
		},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("delegate")})
	deltas := collectDeltas(stream)
	stream.Wait()

	// Should see ToolExecDelta with inner TextContentDelta from child
	execDeltas := collectDeltasByType[types.ToolExecDelta](deltas)
	if len(execDeltas) == 0 {
		t.Fatal("expected ToolExecDelta from child agent")
	}

	// Check that child deltas were forwarded
	foundChildText := false
	for _, ed := range execDeltas {
		if tc, ok := ed.Inner.(types.TextContentDelta); ok && tc.Content == "child result" {
			foundChildText = true
		}
	}
	if !foundChildText {
		t.Error("child text content not forwarded as ToolExecDelta")
	}

	// Final text from parent
	text := textFromDeltas(deltas)
	if !strings.Contains(text, "Parent done based on child.") {
		t.Errorf("parent final text = %q", text)
	}
}

func TestSubAgentRegisteredAsTool(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "hi"},
		SystemPrompt: "sys",
		SubAgents: []SubAgentDef{
			{Name: "helper", Description: "helps", Provider: &mockProvider{response: "ok"}},
		},
	})

	// Tool should be registered as delegate_to_helper
	tool, found := agent.tools.Get("delegate_to_helper")
	if !found {
		t.Fatal("delegate_to_helper not found in registry")
	}

	// Should implement SubAgentInvoker
	if _, ok := tool.(SubAgentInvoker); !ok {
		t.Error("subagent tool does not implement SubAgentInvoker")
	}

	// Tool definition should have "task" parameter
	def := tool.Definition()
	if def.Name != "delegate_to_helper" {
		t.Errorf("tool name = %s, want delegate_to_helper", def.Name)
	}
	if _, ok := def.Parameters.Properties["task"]; !ok {
		t.Error("tool def missing 'task' parameter")
	}
}

func TestSubAgentBlockingExecute(t *testing.T) {
	childProvider := &mockProvider{response: "child output"}

	sat := &subAgentTool{
		def: types.ToolDef{Name: "test_sub", Description: "test"},
		factory: func() *Agent {
			return NewAgent(AgentConfig{
				Provider:     childProvider,
				SystemPrompt: "child",
			})
		},
	}

	result, err := sat.Execute(context.Background(), map[string]any{"task": "do it"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "child output" {
		t.Errorf("result = %q, want 'child output'", result)
	}
}

func TestNestedSubAgents(t *testing.T) {
	// Grandchild provider
	grandchildProvider := &mockProvider{response: "grandchild says hi"}

	// Child calls delegate_to_grandchild, then returns text
	childProvider := &toolCallProvider{
		toolName: "delegate_to_grandchild",
		toolID:   "gc-call",
		toolArgs: map[string]any{"task": "nested task"},
		response: "child relayed grandchild",
	}

	// Parent calls delegate_to_child, then returns text
	parentProvider := &toolCallProvider{
		toolName: "delegate_to_child",
		toolID:   "c-call",
		toolArgs: map[string]any{"task": "delegate deeper"},
		response: "parent done",
	}

	agent := NewAgent(AgentConfig{
		Provider:     parentProvider,
		SystemPrompt: "parent",
		SubAgents: []SubAgentDef{
			{
				Name:         "child",
				Description:  "child",
				SystemPrompt: "child sys",
				Provider:     childProvider,
				SubAgents: []SubAgentDef{
					{
						Name:         "grandchild",
						Description:  "grandchild",
						SystemPrompt: "gc sys",
						Provider:     grandchildProvider,
					},
				},
			},
		},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go deep")})
	collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_ = textFromDeltas(collectDeltas(agent.Invoke(context.Background(), []types.Message{})))
	// Just verify it didn't crash
}

// ===================================================================
// Compactor Integration with Agent
// ===================================================================

func TestAgentWithNoopCompactor(t *testing.T) {
	recording := &recordingProvider{response: "hi"}

	agent := NewAgent(AgentConfig{
		Provider:     recording,
		SystemPrompt: "sys",
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactNone},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hello")})
	collectDeltas(stream)
	stream.Wait()

	recording.mu.Lock()
	if len(recording.calls) != 1 {
		t.Fatalf("provider called %d times, want 1", len(recording.calls))
	}
	// NoopCompactor should pass all messages through unchanged
	if len(recording.calls[0]) != 2 { // system + user
		t.Errorf("messages sent to provider = %d, want 2", len(recording.calls[0]))
	}
	recording.mu.Unlock()
}

func TestAgentWithSlidingWindowCompactor(t *testing.T) {
	recording := &recordingProvider{response: "reply"}

	// Build a tree with several messages already
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()
	current := root
	for i := 0; i < 10; i++ {
		var msg types.Message
		if i%2 == 0 {
			msg = types.NewUserMessage(fmt.Sprintf("user-%d", i))
		} else {
			msg = types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: fmt.Sprintf("asst-%d", i)}}}
		}
		node, _ := tr.AddChild(context.Background(), current.ID, msg)
		current = node
	}

	agent := NewAgent(AgentConfig{
		Provider:   recording,
		CompactCfg: &types.CompactConfig{Strategy: types.CompactSlidingWindow, WindowSize: 3},
		Tree:       tr,
	})

	stream := agent.Invoke(context.Background(), []types.Message{})
	collectDeltas(stream)
	stream.Wait()

	recording.mu.Lock()
	// Should have compacted: system + last 3 = 4 messages
	if len(recording.calls[0]) != 4 {
		t.Errorf("messages after sliding window = %d, want 4", len(recording.calls[0]))
	}
	// First should be system
	if recording.calls[0][0].Role() != types.RoleSystem {
		t.Error("first message should be system")
	}
	recording.mu.Unlock()
}

func TestSlidingWindowCompactorBelowWindow(t *testing.T) {
	compactor := types.NewSlidingWindowCompactor(5)
	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
		types.NewUserMessage("two"),
	}

	result, err := compactor.Compact(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("messages = %d, want 3 (no compaction)", len(result))
	}
}

func TestSummarizeCompactorBelowThreshold(t *testing.T) {
	compactor := types.NewSummarizeCompactor(10)
	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
	}

	result, err := compactor.Compact(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("messages = %d, want 2 (no compaction)", len(result))
	}
}

func TestSummarizeCompactorAboveThreshold(t *testing.T) {
	provider := &mockProvider{response: "conversation summary"}
	compactor := types.NewSummarizeCompactor(3)

	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
		types.NewUserMessage("two"),
		types.NewUserMessage("three"),
		types.NewUserMessage("four"),
		types.NewUserMessage("five"),
	}

	result, err := compactor.Compact(context.Background(), msgs, provider)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Should be: system + summary + last 4 = 6
	if len(result) != 6 {
		t.Errorf("compacted length = %d, want 6", len(result))
	}

	// First should be system
	if result[0].Role() != types.RoleSystem {
		t.Error("first message should be system")
	}
	// Second should be summary
	um, ok := result[1].(types.UserMessage)
	if !ok {
		t.Fatal("second message should be UserMessage (summary)")
	}
	tc, ok := um.Content[0].(types.TextContent)
	if !ok {
		t.Fatal("summary content should be TextContent")
	}
	if !strings.Contains(tc.Text, "conversation summary") {
		t.Errorf("summary text = %q, expected to contain provider response", tc.Text)
	}
}

func TestSummarizeCompactorProviderError(t *testing.T) {
	provider := &errorProvider{err: errors.New("provider down")}
	compactor := types.NewSummarizeCompactor(2)

	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
		types.NewUserMessage("two"),
		types.NewUserMessage("three"),
	}

	// Provider error should cause fallback to original messages
	result, err := compactor.Compact(context.Background(), msgs, provider)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result) != 4 {
		t.Errorf("messages = %d, want 4 (fallback)", len(result))
	}
}

func TestAgentCompactorErrorSilentlyIgnored(t *testing.T) {
	recording := &recordingProvider{response: "hi"}

	agent := NewAgent(AgentConfig{
		Provider:     recording,
		SystemPrompt: "sys",
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 2},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hello")})
	collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should still work -- agent proceeds regardless of compaction outcome.
	recording.mu.Lock()
	if len(recording.calls) < 1 {
		t.Fatalf("provider should have been called at least once")
	}
	recording.mu.Unlock()
}

// ===================================================================
// DefaultAggregator
// ===================================================================

func TestAggregatorTextOnly(t *testing.T) {
	agg := NewDefaultAggregator()
	agg.Push(types.TextStartDelta{})
	agg.Push(types.TextContentDelta{Content: "Hello "})
	agg.Push(types.TextContentDelta{Content: "World"})
	agg.Push(types.TextEndDelta{})

	msg := agg.Message()
	am, ok := msg.(types.AssistantMessage)
	if !ok {
		t.Fatal("expected AssistantMessage")
	}
	if len(am.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(am.Content))
	}
	tc, ok := am.Content[0].(types.TextContent)
	if !ok {
		t.Fatal("expected TextContent")
	}
	if tc.Text != "Hello World" {
		t.Errorf("text = %q, want 'Hello World'", tc.Text)
	}
}

func TestAggregatorToolCallOnly(t *testing.T) {
	agg := NewDefaultAggregator()
	agg.Push(types.ToolCallStartDelta{ID: "tc-1", Name: "search"})
	agg.Push(types.ToolCallArgumentDelta{Content: `{"q": "test"}`})
	agg.Push(types.ToolCallEndDelta{Arguments: map[string]any{"q": "test"}})

	msg := agg.Message()
	am, ok := msg.(types.AssistantMessage)
	if !ok {
		t.Fatal("expected AssistantMessage")
	}
	if len(am.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(am.Content))
	}
	tuc, ok := am.Content[0].(types.ToolUseContent)
	if !ok {
		t.Fatal("expected ToolUseContent")
	}
	if tuc.ID != "tc-1" || tuc.Name != "search" {
		t.Errorf("tool call = %+v", tuc)
	}
}

func TestAggregatorMixedTextAndToolCalls(t *testing.T) {
	agg := NewDefaultAggregator()

	// Text first
	agg.Push(types.TextStartDelta{})
	agg.Push(types.TextContentDelta{Content: "Let me search"})
	agg.Push(types.TextEndDelta{})

	// Then tool call
	agg.Push(types.ToolCallStartDelta{ID: "tc-1", Name: "search"})
	agg.Push(types.ToolCallEndDelta{Arguments: map[string]any{"q": "test"}})

	msg := agg.Message()
	am := msg.(types.AssistantMessage)
	if len(am.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(am.Content))
	}
	if _, ok := am.Content[0].(types.TextContent); !ok {
		t.Error("first block should be TextContent")
	}
	if _, ok := am.Content[1].(types.ToolUseContent); !ok {
		t.Error("second block should be ToolUseContent")
	}
}

func TestAggregatorEmptyReturnsNil(t *testing.T) {
	agg := NewDefaultAggregator()
	if msg := agg.Message(); msg != nil {
		t.Errorf("expected nil, got %v", msg)
	}
}

func TestAggregatorReset(t *testing.T) {
	agg := NewDefaultAggregator()
	agg.Push(types.TextStartDelta{})
	agg.Push(types.TextContentDelta{Content: "hello"})
	agg.Push(types.TextEndDelta{})

	agg.Reset()
	if msg := agg.Message(); msg != nil {
		t.Error("expected nil after reset")
	}
}

func TestAggregatorInProgressText(t *testing.T) {
	agg := NewDefaultAggregator()
	agg.Push(types.TextStartDelta{})
	agg.Push(types.TextContentDelta{Content: "partial"})
	// No TextEndDelta -- in-progress

	msg := agg.Message()
	am := msg.(types.AssistantMessage)
	tc := am.Content[0].(types.TextContent)
	if tc.Text != "partial" {
		t.Errorf("in-progress text = %q, want 'partial'", tc.Text)
	}
}

func TestAggregatorIgnoresNonTextNonToolDeltas(t *testing.T) {
	agg := NewDefaultAggregator()
	agg.Push(types.ErrorDelta{Error: errors.New("ignored")})
	agg.Push(types.DoneDelta{})
	agg.Push(types.ToolExecStartDelta{})
	agg.Push(types.ToolExecEndDelta{})

	if msg := agg.Message(); msg != nil {
		t.Error("expected nil, aggregator should ignore non-text/tool deltas")
	}
}

// ===================================================================
// Replay
// ===================================================================

func TestReplayAssistantText(t *testing.T) {
	messages := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("hello"),
		types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "hi there"}}},
	}

	stream := Replay(messages)
	deltas := collectDeltas(stream)
	stream.Wait()

	// System and user messages produce no deltas; assistant text produces TextStart/Content/End + Done
	starts := collectDeltasByType[types.TextStartDelta](deltas)
	contents := collectDeltasByType[types.TextContentDelta](deltas)
	ends := collectDeltasByType[types.TextEndDelta](deltas)
	dones := collectDeltasByType[types.DoneDelta](deltas)

	if len(starts) != 1 {
		t.Errorf("TextStartDelta count = %d, want 1", len(starts))
	}
	if len(contents) != 1 || contents[0].Content != "hi there" {
		t.Errorf("content = %v, want 'hi there'", contents)
	}
	if len(ends) != 1 {
		t.Errorf("TextEndDelta count = %d, want 1", len(ends))
	}
	if len(dones) != 1 {
		t.Errorf("DoneDelta count = %d, want 1", len(dones))
	}
}

func TestReplayAssistantToolUse(t *testing.T) {
	messages := []types.Message{
		types.AssistantMessage{Content: []types.AssistantContent{
			types.ToolUseContent{ID: "tc-1", Name: "search", Arguments: map[string]any{"q": "test"}},
		}},
	}

	stream := Replay(messages)
	deltas := collectDeltas(stream)
	stream.Wait()

	toolStarts := collectDeltasByType[types.ToolCallStartDelta](deltas)
	toolEnds := collectDeltasByType[types.ToolCallEndDelta](deltas)

	if len(toolStarts) != 1 || toolStarts[0].ID != "tc-1" || toolStarts[0].Name != "search" {
		t.Errorf("ToolCallStartDelta = %+v", toolStarts)
	}
	if len(toolEnds) != 1 {
		t.Errorf("ToolCallEndDelta count = %d, want 1", len(toolEnds))
	}
}

func TestReplayToolResults(t *testing.T) {
	messages := []types.Message{
		types.NewToolResultMessage(
			types.ToolResultContent{ToolCallID: "tc-1", Text: "result1"},
			types.ToolResultContent{ToolCallID: "tc-2", Text: "result2"},
		),
	}

	stream := Replay(messages)
	deltas := collectDeltas(stream)
	stream.Wait()

	execStarts := collectDeltasByType[types.ToolExecStartDelta](deltas)
	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)

	if len(execStarts) != 2 {
		t.Errorf("ToolExecStartDelta count = %d, want 2", len(execStarts))
	}
	if len(execEnds) != 2 {
		t.Errorf("ToolExecEndDelta count = %d, want 2", len(execEnds))
	}
}

func TestReplayUserToolResults(t *testing.T) {
	messages := []types.Message{
		types.NewUserToolResultMessage(types.ToolResultContent{ToolCallID: "tc-1", Text: "user result"}),
	}

	stream := Replay(messages)
	deltas := collectDeltas(stream)
	stream.Wait()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 1 {
		t.Errorf("ToolExecEndDelta count = %d, want 1", len(execEnds))
	}
	if execEnds[0].Result != "user result" {
		t.Errorf("result = %q, want 'user result'", execEnds[0].Result)
	}
}

func TestReplayMixedConversation(t *testing.T) {
	messages := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("hello"),
		types.AssistantMessage{Content: []types.AssistantContent{
			types.TextContent{Text: "I'll search"},
			types.ToolUseContent{ID: "tc-1", Name: "search", Arguments: map[string]any{}},
		}},
		types.NewToolResultMessage(types.ToolResultContent{ToolCallID: "tc-1", Text: "found it"}),
		types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "Here is the result"}}},
	}

	stream := Replay(messages)
	deltas := collectDeltas(stream)
	stream.Wait()

	// 2 text blocks (from 2 assistant messages) + 1 tool use + 1 tool result
	textStarts := collectDeltasByType[types.TextStartDelta](deltas)
	toolCallStarts := collectDeltasByType[types.ToolCallStartDelta](deltas)
	execStarts := collectDeltasByType[types.ToolExecStartDelta](deltas)

	if len(textStarts) != 2 {
		t.Errorf("TextStartDelta count = %d, want 2", len(textStarts))
	}
	if len(toolCallStarts) != 1 {
		t.Errorf("ToolCallStartDelta count = %d, want 1", len(toolCallStarts))
	}
	if len(execStarts) != 1 {
		t.Errorf("ToolExecStartDelta count = %d, want 1", len(execStarts))
	}
}

func TestReplayEmptyMessages(t *testing.T) {
	stream := Replay(nil)
	deltas := collectDeltas(stream)
	stream.Wait()

	// Only DoneDelta expected
	if len(deltas) != 1 {
		t.Errorf("deltas = %d, want 1 (DoneDelta only)", len(deltas))
	}
	if _, ok := deltas[0].(types.DoneDelta); !ok {
		t.Error("expected DoneDelta")
	}
}

// ===================================================================
// ToolRegistry
// ===================================================================

func TestToolRegistryBasicOps(t *testing.T) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "test_tool", Description: "test"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			return "ok", nil
		},
	}

	reg := types.NewToolRegistry(tool)

	// Get
	found, ok := reg.Get("test_tool")
	if !ok {
		t.Fatal("tool not found")
	}
	if found.Definition().Name != "test_tool" {
		t.Error("wrong tool returned")
	}

	// Get nonexistent
	_, ok = reg.Get("nope")
	if ok {
		t.Error("should not find nonexistent tool")
	}

	// Definitions
	defs := reg.Definitions()
	if len(defs) != 1 {
		t.Errorf("definitions count = %d, want 1", len(defs))
	}

	// Execute
	result, err := reg.Execute(context.Background(), "test_tool", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %q, want 'ok'", result)
	}

	// Execute nonexistent
	_, err = reg.Execute(context.Background(), "nope", nil)
	if !errors.Is(err, types.ErrToolNotFound) {
		t.Errorf("expected ErrToolNotFound, got %v", err)
	}
}

func TestToolRegistryRegister(t *testing.T) {
	reg := types.NewToolRegistry()

	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "added", Description: "added later"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			return "added result", nil
		},
	}

	reg.Register(tool)
	_, ok := reg.Get("added")
	if !ok {
		t.Error("registered tool not found")
	}
}

func TestToolRegistryOverwrite(t *testing.T) {
	tool1 := &types.ToolFunc{
		Def: types.ToolDef{Name: "tool", Description: "v1"},
		Fn:  func(_ context.Context, _ map[string]any) (string, error) { return "v1", nil },
	}
	tool2 := &types.ToolFunc{
		Def: types.ToolDef{Name: "tool", Description: "v2"},
		Fn:  func(_ context.Context, _ map[string]any) (string, error) { return "v2", nil },
	}

	reg := types.NewToolRegistry(tool1)
	reg.Register(tool2)

	result, _ := reg.Execute(context.Background(), "tool", nil)
	if result != "v2" {
		t.Errorf("result = %q, want 'v2' (overwritten)", result)
	}
}

func TestEmptyToolRegistry(t *testing.T) {
	reg := types.NewToolRegistry()

	defs := reg.Definitions()
	if len(defs) != 0 {
		t.Errorf("definitions = %d, want 0", len(defs))
	}
}

// ===================================================================
// Tree + Agent: Invoke tests (moved from tree_test.go)
// ===================================================================

func TestInvoke(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("You are helpful."))

	provider := &mockProvider{response: "Hello!"}

	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "You are helpful.",
		Provider:     provider,
		Tree:         tr,
	})

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("Hi"),
	})

	// Consume all deltas.
	for range stream.Deltas() {
	}

	if err := stream.Wait(); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Verify tree has the conversation persisted.
	msgs, err := tr.FlattenBranch("main")
	if err != nil {
		t.Fatalf("FlattenBranch: %v", err)
	}

	// Should have: system + user("Hi") + assistant("Hello!")
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	if msgs[0].Role() != types.RoleSystem {
		t.Error("msgs[0] not system")
	}
	if msgs[1].Role() != types.RoleUser {
		t.Error("msgs[1] not user")
	}
	if msgs[2].Role() != types.RoleAssistant {
		t.Error("msgs[2] not assistant")
	}
}

func TestInvokeOnExplicitBranch(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("You are helpful."))
	root := tr.Root()

	// Set up a side branch.
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("setup"))
	asst, _ := tr.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "ok"}},
	})
	branchID, _, _ := tr.Branch(context.Background(), asst.ID, "side", types.NewUserMessage("side question"))

	provider := &mockProvider{response: "side answer"}

	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "You are helpful.",
		Provider:     provider,
		Tree:         tr,
	})

	stream := agent.Invoke(context.Background(), []types.Message{}, branchID)
	for range stream.Deltas() {
	}
	stream.Wait()

	msgs, _ := tr.FlattenBranch(branchID)
	// system + setup user + setup asst + side question + side answer
	if len(msgs) != 5 {
		t.Fatalf("messages = %d, want 5", len(msgs))
	}
}

func TestInvokeAutoCreatesTree(t *testing.T) {
	provider := &mockProvider{response: "Hello!"}

	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "You are helpful.",
		Provider:     provider,
	})

	// Tree should be auto-created.
	if agent.Tree() == nil {
		t.Fatal("expected auto-created tree")
	}

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("Hi"),
	})

	for range stream.Deltas() {
	}

	if err := stream.Wait(); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	msgs, err := agent.Tree().FlattenBranch("main")
	if err != nil {
		t.Fatalf("FlattenBranch: %v", err)
	}
	if len(msgs) != 3 { // system + user + assistant
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
}

func TestInvokeUsesActiveCursor(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("You are helpful."))
	root := tr.Root()

	// Create a side branch
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("setup"))
	branchID, _, _ := tr.Branch(context.Background(), user.ID, "side", types.NewUserMessage("side msg"))

	// Set side as active
	tr.SetActive(branchID)

	provider := &mockProvider{response: "side answer"}
	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "You are helpful.",
		Provider:     provider,
		Tree:         tr,
	})

	// Invoke without explicit branch -- should use active (side)
	stream := agent.Invoke(context.Background(), []types.Message{})
	for range stream.Deltas() {
	}
	stream.Wait()

	msgs, _ := tr.FlattenBranch(branchID)
	// system + setup user + side msg + side answer
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	if msgs[3].Role() != types.RoleAssistant {
		t.Error("last message should be assistant")
	}
}

// ===================================================================
// Tree + Agent Integration
// ===================================================================

func TestAgentMultipleInvocationsOnSameTree(t *testing.T) {
	provider := &mockProvider{response: "response"}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	// First conversation turn
	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("turn 1")})
	collectDeltas(stream)
	stream.Wait()

	// Second conversation turn (continues on same branch)
	stream = agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("turn 2")})
	collectDeltas(stream)
	stream.Wait()

	msgs, _ := agent.Tree().FlattenBranch("main")
	// system + user1 + asst1 + user2 + asst2 = 5
	if len(msgs) != 5 {
		t.Fatalf("messages = %d, want 5", len(msgs))
	}
	if msgs[3].Role() != types.RoleUser {
		t.Error("msgs[3] should be user (turn 2)")
	}
	if msgs[4].Role() != types.RoleAssistant {
		t.Error("msgs[4] should be assistant (turn 2)")
	}
}

func TestAgentInvokeWithMultipleInputMessages(t *testing.T) {
	provider := &mockProvider{response: "got both"}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("first"),
		types.NewUserMessage("second"),
	})
	collectDeltas(stream)
	stream.Wait()

	msgs, _ := agent.Tree().FlattenBranch("main")
	// system + user1 + user2 + assistant = 4
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
}

func TestAgentBranchAndContinue(t *testing.T) {
	provider := &mockProvider{response: "branched response"}

	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tr.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Branch from assistant
	branchID, _, _ := tr.Branch(context.Background(), asst.ID, "edit", types.NewUserMessage("different question"))

	agent := NewAgent(AgentConfig{
		Provider: provider,
		Tree:     tr,
	})

	// Invoke on the branch
	stream := agent.Invoke(context.Background(), []types.Message{}, branchID)
	collectDeltas(stream)
	stream.Wait()

	// Branch should have: sys + hello + hi + different question + branched response
	msgs, _ := tr.FlattenBranch(branchID)
	if len(msgs) != 5 {
		t.Fatalf("branch messages = %d, want 5", len(msgs))
	}

	// Main should still be: sys + hello + hi
	mainMsgs, _ := tr.FlattenBranch("main")
	if len(mainMsgs) != 3 {
		t.Errorf("main messages = %d, want 3", len(mainMsgs))
	}
}

func TestAgentInvokeOnNonExistentBranch(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "hi"},
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")}, "nonexistent")
	deltas := collectDeltas(stream)
	stream.Wait()

	// Should get an ErrorDelta for branch not found
	errorDeltas := collectDeltasByType[types.ErrorDelta](deltas)
	if len(errorDeltas) == 0 {
		t.Fatal("expected ErrorDelta for nonexistent branch")
	}
}

// ===================================================================
// EventStream Edge Cases
// ===================================================================

func TestEventStreamDrainRequired(t *testing.T) {
	provider := &mockProvider{response: "hi"}
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})

	// Must drain before Wait completes (DoneDelta is sent to channel)
	done := make(chan error)
	go func() {
		done <- stream.Wait()
	}()

	// Drain
	for range stream.Deltas() {
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after draining")
	}
}

// ===================================================================
// Message Constructors and Content Types
// ===================================================================

func TestMessageConstructors(t *testing.T) {
	sys := types.NewSystemMessage("system prompt")
	if sys.Role() != types.RoleSystem {
		t.Error("system role wrong")
	}
	if len(sys.Content) != 1 {
		t.Fatal("system content length != 1")
	}
	if tc, ok := sys.Content[0].(types.TextContent); !ok || tc.Text != "system prompt" {
		t.Error("system text wrong")
	}

	usr := types.NewUserMessage("user input")
	if usr.Role() != types.RoleUser {
		t.Error("user role wrong")
	}
	if tc, ok := usr.Content[0].(types.TextContent); !ok || tc.Text != "user input" {
		t.Error("user text wrong")
	}

	tr := types.NewToolResultMessage(
		types.ToolResultContent{ToolCallID: "tc-1", Text: "result"},
	)
	if tr.Role() != types.RoleSystem {
		t.Error("tool result message role should be system")
	}
	if trc, ok := tr.Content[0].(types.ToolResultContent); !ok || trc.ToolCallID != "tc-1" {
		t.Error("tool result content wrong")
	}

	utr := types.NewUserToolResultMessage(
		types.ToolResultContent{ToolCallID: "tc-2", Text: "user result"},
	)
	if utr.Role() != types.RoleUser {
		t.Error("user tool result role should be user")
	}
	if trc, ok := utr.Content[0].(types.ToolResultContent); !ok || trc.ToolCallID != "tc-2" {
		t.Error("user tool result content wrong")
	}
}

func TestToolResultContentInSystemMessage(t *testing.T) {
	msg := types.NewToolResultMessage(
		types.ToolResultContent{ToolCallID: "a", Text: "result-a"},
		types.ToolResultContent{ToolCallID: "b", Text: "result-b"},
	)

	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(msg.Content))
	}

	for i, c := range msg.Content {
		trc, ok := c.(types.ToolResultContent)
		if !ok {
			t.Fatalf("content[%d] not ToolResultContent", i)
		}
		if trc.ToolCallID == "" {
			t.Errorf("content[%d] has empty ToolCallID", i)
		}
	}
}

// ===================================================================
// WAL Integration
// ===================================================================

func TestWALMultipleTransactions(t *testing.T) {
	wal := memwal.New()

	ctx := context.Background()

	// Multiple committed transactions
	for i := 0; i < 5; i++ {
		txID, _ := wal.Begin(ctx)
		wal.Append(ctx, txID, types.TxOp{Kind: types.TxOpAddNode})
		wal.Commit(ctx, txID)
	}

	committed, _ := wal.Recover(ctx)
	if len(committed) != 5 {
		t.Errorf("committed = %d, want 5", len(committed))
	}
}

func TestWALAbortedNotRecovered(t *testing.T) {
	wal := memwal.New()

	ctx := context.Background()
	txID, _ := wal.Begin(ctx)
	wal.Append(ctx, txID, types.TxOp{Kind: types.TxOpAddNode})
	wal.Abort(ctx, txID)

	committed, _ := wal.Recover(ctx)
	if len(committed) != 0 {
		t.Errorf("committed = %d, want 0 (all aborted)", len(committed))
	}
}

func TestWALReplayNonexistent(t *testing.T) {
	wal := memwal.New()
	_, err := wal.Replay(context.Background(), "nonexistent-tx")
	if err == nil {
		t.Error("expected error for nonexistent tx")
	}
}

func TestTreeBranchWithWAL(t *testing.T) {
	wal := memwal.New()
	tr, _ := tree.New(types.NewSystemMessage("sys"), tree.WithWAL(wal))
	root := tr.Root()

	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	tr.Branch(context.Background(), user.ID, "alt", types.NewUserMessage("branch msg"))

	committed, _ := wal.Recover(context.Background())
	// AddChild(hello) + Branch(branch msg) = 2 transactions
	if len(committed) != 2 {
		t.Errorf("committed = %d, want 2", len(committed))
	}
}

func TestTreeUpdateUserMessageWithWAL(t *testing.T) {
	wal := memwal.New()
	tr, _ := tree.New(types.NewSystemMessage("sys"), tree.WithWAL(wal))
	root := tr.Root()

	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("original"))
	tr.UpdateUserMessage(context.Background(), user.ID, types.NewUserMessage("edited"))

	committed, _ := wal.Recover(context.Background())
	// AddChild + UpdateUserMessage = 2 transactions
	if len(committed) != 2 {
		t.Errorf("committed = %d, want 2", len(committed))
	}
}

// ===================================================================
// Concurrent Agent Operations
// ===================================================================

func TestConcurrentInvocationsOnDifferentBranches(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("shared"))
	asst, _ := tr.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "shared reply"}},
	})

	// Create multiple branches
	branches := make([]types.BranchID, 5)
	for i := range branches {
		bid, _, _ := tr.Branch(context.Background(), asst.ID, fmt.Sprintf("branch-%d", i), types.NewUserMessage(fmt.Sprintf("branch %d input", i)))
		branches[i] = bid
	}

	provider := &mockProvider{response: "branch response"}
	agent := NewAgent(AgentConfig{
		Provider: provider,
		Tree:     tr,
	})

	// Invoke on all branches concurrently
	var wg sync.WaitGroup
	for _, b := range branches {
		wg.Add(1)
		go func(bid types.BranchID) {
			defer wg.Done()
			stream := agent.Invoke(context.Background(), []types.Message{}, bid)
			collectDeltas(stream)
			stream.Wait()
		}(b)
	}
	wg.Wait()

	// Each branch should have: sys + shared + shared reply + branch input + branch response = 5
	for _, b := range branches {
		msgs, err := tr.FlattenBranch(b)
		if err != nil {
			t.Errorf("FlattenBranch(%s): %v", b, err)
			continue
		}
		if len(msgs) != 5 {
			t.Errorf("branch %s messages = %d, want 5", b, len(msgs))
		}
	}
}

// ===================================================================
// MessagesToText
// ===================================================================

func TestMessagesToText(t *testing.T) {
	msgs := []types.Message{
		types.NewSystemMessage("system prompt"),
		types.NewUserMessage("user question"),
		types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "assistant reply"}}},
		types.NewToolResultMessage(types.ToolResultContent{ToolCallID: "tc-1", Text: "tool output"}),
	}

	text := types.MessagesToText(msgs)

	if !strings.Contains(text, "System: system prompt") {
		t.Error("missing system text")
	}
	if !strings.Contains(text, "User: user question") {
		t.Error("missing user text")
	}
	if !strings.Contains(text, "Assistant: assistant reply") {
		t.Error("missing assistant text")
	}
	if !strings.Contains(text, "Tool Result [tc-1]: tool output") {
		t.Error("missing tool result text")
	}
}

func TestMessagesToTextUserToolResult(t *testing.T) {
	msgs := []types.Message{
		types.NewUserToolResultMessage(types.ToolResultContent{ToolCallID: "tc-1", Text: "user tool"}),
	}

	text := types.MessagesToText(msgs)
	if !strings.Contains(text, "Tool Result [tc-1]: user tool") {
		t.Error("missing user tool result text")
	}
}

// ===================================================================
// Edge Cases: Agent with empty/unusual inputs
// ===================================================================

func TestAgentInvokeNoInputMessages(t *testing.T) {
	provider := &mockProvider{response: "unprompted"}
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{})
	deltas := collectDeltas(stream)
	stream.Wait()

	got := textFromDeltas(deltas)
	if got != "unprompted" {
		t.Errorf("text = %q, want 'unprompted'", got)
	}

	// Tree should have: system + assistant
	msgs, _ := agent.Tree().FlattenBranch("main")
	if len(msgs) != 2 {
		t.Errorf("messages = %d, want 2", len(msgs))
	}
}

func TestAgentInvokeNilInputMessages(t *testing.T) {
	provider := &mockProvider{response: "hi"}
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), nil)
	collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentEmptySystemPrompt(t *testing.T) {
	provider := &mockProvider{response: "hi"}
	agent := NewAgent(AgentConfig{
		Provider: provider,
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hello")})
	collectDeltas(stream)
	stream.Wait()

	msgs, _ := agent.Tree().FlattenBranch("main")
	// Even with empty system prompt, root is a SystemMessage
	if msgs[0].Role() != types.RoleSystem {
		t.Error("first message should be system even with empty prompt")
	}
}

// ===================================================================
// Tree: Archive edge cases (public API only)
// ===================================================================

func TestArchiveNonRecursive(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tr.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Archive user non-recursively
	tr.Archive(user.ID, "test", false)

	// Flatten from asst should skip archived user but include asst
	msgs, _ := tr.Flatten(asst.ID)
	// root + asst (user is archived)
	if len(msgs) != 2 {
		t.Errorf("flatten after non-recursive archive = %d, want 2", len(msgs))
	}
}

func TestArchiveNodeNotFound(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))

	err := tr.Archive("nonexistent", "test", false)
	if err == nil {
		t.Error("expected error for nonexistent node")
	}
}

func TestRestoreNodeNotFound(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))

	err := tr.Restore("nonexistent", false)
	if err == nil {
		t.Error("expected error for nonexistent node")
	}
}

// ===================================================================
// Tree: Deep branching and multi-branch operations
// ===================================================================

func TestDeepConversationTree(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	// Build a deep chain of 50 messages
	current := root
	for i := 0; i < 50; i++ {
		var msg types.Message
		if i%2 == 0 {
			msg = types.NewUserMessage(fmt.Sprintf("user-%d", i))
		} else {
			msg = types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: fmt.Sprintf("asst-%d", i)}}}
		}
		node, err := tr.AddChild(context.Background(), current.ID, msg)
		if err != nil {
			t.Fatalf("AddChild %d: %v", i, err)
		}
		current = node
	}

	// Flatten should return all 51 messages (root + 50)
	msgs, err := tr.FlattenBranch("main")
	if err != nil {
		t.Fatalf("FlattenBranch: %v", err)
	}
	if len(msgs) != 51 {
		t.Errorf("messages = %d, want 51", len(msgs))
	}

	// NodePath should have depth 50
	path, _ := tr.NodePath(current.ID)
	if len(path) != 50 {
		t.Errorf("path length = %d, want 50", len(path))
	}
}

func TestMultipleBranchesFromSameNode(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	// Create 5 branches from the same node
	branchIDs := make([]types.BranchID, 5)
	for i := range 5 {
		bid, _, err := tr.Branch(context.Background(), user.ID, fmt.Sprintf("branch-%d", i), types.NewUserMessage(fmt.Sprintf("alt-%d", i)))
		if err != nil {
			t.Fatalf("Branch %d: %v", i, err)
		}
		branchIDs[i] = bid
	}

	// Each branch should flatten independently
	for i, bid := range branchIDs {
		msgs, _ := tr.FlattenBranch(bid)
		// sys + hello + alt-i = 3
		if len(msgs) != 3 {
			t.Errorf("branch %d messages = %d, want 3", i, len(msgs))
		}
	}

	// Children of user should be 5 (the branch nodes)
	children, _ := tr.Children(user.ID)
	if len(children) != 5 {
		t.Errorf("children = %d, want 5", len(children))
	}
}

func TestBranchNameCollision(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	// Create two branches with the same name
	bid1, _, err := tr.Branch(context.Background(), user.ID, "same", types.NewUserMessage("first"))
	if err != nil {
		t.Fatalf("Branch 1: %v", err)
	}
	bid2, _, err := tr.Branch(context.Background(), user.ID, "same", types.NewUserMessage("second"))
	if err != nil {
		t.Fatalf("Branch 2: %v", err)
	}

	// They should have different IDs due to dedup suffix
	if bid1 == bid2 {
		t.Error("branch IDs should differ despite same name")
	}

	// Both should be valid branches
	branches := tr.Branches()
	if _, ok := branches[bid1]; !ok {
		t.Error("branch 1 not found")
	}
	if _, ok := branches[bid2]; !ok {
		t.Error("branch 2 not found")
	}
}

// ===================================================================
// Tree Compaction Integration
// ===================================================================

func TestTreeCompactChangesActiveBranch(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	current := root
	for i := 0; i < 10; i++ {
		var msg types.Message
		if i%2 == 0 {
			msg = types.NewUserMessage(fmt.Sprintf("user-%d", i))
		} else {
			msg = types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: fmt.Sprintf("asst-%d", i)}}}
		}
		node, _ := tr.AddChild(context.Background(), current.ID, msg)
		current = node
	}

	if tr.Active() != "main" {
		t.Fatal("active should be main before compaction")
	}

	provider := &mockProvider{response: "summary"}
	tokenizer := &mockTokenizer{tokensPerMessage: 100}

	newBranch, err := tr.Compact(context.Background(), "main", provider, tokenizer, tree.CompactOpts{
		MaxTokens: 500,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if tr.Active() != newBranch {
		t.Errorf("active = %s, want %s (compacted branch)", tr.Active(), newBranch)
	}
	if newBranch == "main" {
		t.Error("compacted branch should be different from main")
	}
}

func TestTreeCompactOriginalBranchIntact(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	current := root
	for i := 0; i < 8; i++ {
		msg := types.NewUserMessage(fmt.Sprintf("msg-%d", i))
		node, _ := tr.AddChild(context.Background(), current.ID, msg)
		current = node
	}

	provider := &mockProvider{response: "summary"}
	tokenizer := &mockTokenizer{tokensPerMessage: 100}

	originalMsgs, _ := tr.FlattenBranch("main")
	originalCount := len(originalMsgs)

	tr.Compact(context.Background(), "main", provider, tokenizer, tree.CompactOpts{MaxTokens: 200})

	afterMsgs, _ := tr.FlattenBranch("main")
	if len(afterMsgs) != originalCount {
		t.Errorf("original branch changed: %d -> %d", originalCount, len(afterMsgs))
	}
}

// ===================================================================
// Checkpoint + Rewind + Agent Invoke
// ===================================================================

func TestCheckpointRewindAndInvoke(t *testing.T) {
	provider := &mockProvider{response: "response"}

	tr, _ := tree.New(types.NewSystemMessage("sys"))
	agent := NewAgent(AgentConfig{
		Provider: provider,
		Tree:     tr,
	})

	// First turn
	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("turn 1")})
	collectDeltas(stream)
	stream.Wait()

	// Checkpoint after turn 1 (tip is asst1)
	cpID, err := tr.Checkpoint("main", "after-turn-1")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Second turn on main
	stream = agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("turn 2")})
	collectDeltas(stream)
	stream.Wait()

	// Verify main has both turns: sys + user1 + asst1 + user2 + asst2
	mainMsgs, _ := tr.FlattenBranch("main")
	if len(mainMsgs) != 5 {
		t.Fatalf("main messages = %d, want 5", len(mainMsgs))
	}

	// Rewind to after turn 1
	rewindBranch, err := tr.Rewind(cpID)
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}

	// Verify rewind branch starts at checkpoint: sys + user1 + asst1
	rewindMsgsBefore, _ := tr.FlattenBranch(rewindBranch)
	if len(rewindMsgsBefore) != 3 {
		t.Fatalf("rewind before invoke = %d, want 3", len(rewindMsgsBefore))
	}

	tip, _ := tr.Tip(rewindBranch)
	altBranch, _, err := tr.Branch(context.Background(), tip.ID, "alt-turn-2", types.NewUserMessage("alternate turn 2"))
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	stream = agent.Invoke(context.Background(), []types.Message{}, altBranch)
	collectDeltas(stream)
	stream.Wait()

	// Alt branch: sys + user1 + asst1 + alt_user2 + alt_asst2
	altMsgs, _ := tr.FlattenBranch(altBranch)
	if len(altMsgs) != 5 {
		t.Errorf("alt branch messages = %d, want 5", len(altMsgs))
	}

	// Main should be unchanged at 5
	mainMsgs2, _ := tr.FlattenBranch("main")
	if len(mainMsgs2) != 5 {
		t.Errorf("main messages after alt invoke = %d, want 5", len(mainMsgs2))
	}
}

// ===================================================================
// UpdateUserMessage + Agent Invoke
// ===================================================================

func TestUpdateUserMessageAndInvoke(t *testing.T) {
	provider := &mockProvider{response: "response"}

	tr, _ := tree.New(types.NewSystemMessage("sys"))
	agent := NewAgent(AgentConfig{
		Provider: provider,
		Tree:     tr,
	})

	// First turn
	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("original question")})
	collectDeltas(stream)
	stream.Wait()

	// Find the user node
	msgs, _ := tr.FlattenBranch("main")
	tip, _ := tr.Tip("main")
	path, _ := tr.Path(tip.ID)
	// path[1] should be the user node
	userNodeID := path[1]

	// Edit the user message
	editBranch, _, err := tr.UpdateUserMessage(context.Background(), userNodeID, types.NewUserMessage("edited question"))
	if err != nil {
		t.Fatalf("UpdateUserMessage: %v", err)
	}

	// Invoke on the edit branch
	stream = agent.Invoke(context.Background(), []types.Message{}, editBranch)
	collectDeltas(stream)
	stream.Wait()

	// Edit branch: sys + edited + response = 3
	editMsgs, _ := tr.FlattenBranch(editBranch)
	if len(editMsgs) != 3 {
		t.Errorf("edit messages = %d, want 3", len(editMsgs))
	}
	um := editMsgs[1].(types.UserMessage)
	tc := um.Content[0].(types.TextContent)
	if tc.Text != "edited question" {
		t.Errorf("edited text = %q", tc.Text)
	}

	// Original branch unchanged
	if len(msgs) != 3 {
		t.Errorf("original messages changed: %d", len(msgs))
	}
}

// ===================================================================
// Agent + Tree: Active cursor integration
// ===================================================================

func TestAgentRespectsSetActive(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("setup"))
	bid, _, _ := tr.Branch(context.Background(), user.ID, "alt", types.NewUserMessage("alt setup"))

	tr.SetActive(bid)

	provider := &mockProvider{response: "on alt branch"}
	agent := NewAgent(AgentConfig{
		Provider: provider,
		Tree:     tr,
	})

	// Invoke without explicit branch
	stream := agent.Invoke(context.Background(), []types.Message{})
	collectDeltas(stream)
	stream.Wait()

	// Should have invoked on alt branch
	altMsgs, _ := tr.FlattenBranch(bid)
	// sys + setup + alt setup + response = 4
	if len(altMsgs) != 4 {
		t.Errorf("alt branch messages = %d, want 4", len(altMsgs))
	}
}

// ===================================================================
// Diff integration
// ===================================================================

func TestDiffAfterAgentInvoke(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("shared"))
	asst, _ := tr.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "shared reply"}},
	})

	// Branch
	bid, _, _ := tr.Branch(context.Background(), asst.ID, "alt", types.NewUserMessage("alt question"))

	// Invoke on both branches
	provider := &mockProvider{response: "reply"}
	agent := NewAgent(AgentConfig{Provider: provider, Tree: tr})

	s1 := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("main q")})
	collectDeltas(s1)
	s1.Wait()

	s2 := agent.Invoke(context.Background(), []types.Message{}, bid)
	collectDeltas(s2)
	s2.Wait()

	diff, err := tr.DiffBranches("main", bid)
	if err != nil {
		t.Fatalf("DiffBranches: %v", err)
	}

	if diff.CommonAncestor != asst.ID {
		t.Error("common ancestor should be the shared assistant node")
	}
	// Main has: main_q + main_reply = 2 unique nodes
	if len(diff.Removed) != 2 {
		t.Errorf("removed = %d, want 2", len(diff.Removed))
	}
	// Alt has: alt_question + alt_reply = 2 unique nodes
	if len(diff.Added) != 2 {
		t.Errorf("added = %d, want 2", len(diff.Added))
	}
}

// ===================================================================
// Full end-to-end scenario
// ===================================================================

func TestEndToEndScenario(t *testing.T) {
	callCount := 0
	searchTool := &types.ToolFunc{
		Def: types.ToolDef{Name: "search", Description: "search the web"},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			callCount++
			q, _ := args["query"].(string)
			return fmt.Sprintf("search results for: %s", q), nil
		},
	}

	// Provider: turn 1 calls search, turn 2 responds with text
	provider := &sequenceProvider{
		responses: []func(ch chan<- types.Delta){
			func(ch chan<- types.Delta) {
				ch <- types.ToolCallStartDelta{ID: "tc-1", Name: "search"}
				ch <- types.ToolCallEndDelta{Arguments: map[string]any{"query": "golang testing"}}
			},
			func(ch chan<- types.Delta) {
				ch <- types.TextStartDelta{}
				ch <- types.TextContentDelta{Content: "Based on search: Go testing is great"}
				ch <- types.TextEndDelta{}
			},
			// Turn 2 (second user input, new invocation)
			func(ch chan<- types.Delta) {
				ch <- types.TextStartDelta{}
				ch <- types.TextContentDelta{Content: "You're welcome!"}
				ch <- types.TextEndDelta{}
			},
		},
	}

	tr, _ := tree.New(types.NewSystemMessage("You are a helpful assistant."))
	agent := NewAgent(AgentConfig{
		Provider: provider,
		Tools:    types.NewToolRegistry(searchTool),
		Tree:     tr,
	})

	// Turn 1
	s1 := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Tell me about Go testing")})
	d1 := collectDeltas(s1)
	s1.Wait()

	if callCount != 1 {
		t.Fatalf("search called %d times, want 1", callCount)
	}

	text1 := textFromDeltas(d1)
	if !strings.Contains(text1, "Go testing is great") {
		t.Errorf("turn 1 text = %q", text1)
	}

	msgs1, _ := tr.FlattenBranch("main")
	if len(msgs1) != 5 {
		t.Fatalf("turn 1 messages = %d, want 5", len(msgs1))
	}

	// Turn 2
	s2 := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Thanks!")})
	d2 := collectDeltas(s2)
	s2.Wait()

	text2 := textFromDeltas(d2)
	if text2 != "You're welcome!" {
		t.Errorf("turn 2 text = %q", text2)
	}

	msgs2, _ := tr.FlattenBranch("main")
	if len(msgs2) != 7 {
		t.Fatalf("turn 2 messages = %d, want 7", len(msgs2))
	}

	// Checkpoint and edit
	cpID, _ := tr.Checkpoint("main", "after-turn-2")

	tip1, _ := tr.Tip("main")
	fullPath, _ := tr.Path(tip1.ID)
	userNodeID := fullPath[1]

	editBranch, _, _ := tr.UpdateUserMessage(context.Background(), userNodeID, types.NewUserMessage("Tell me about Rust testing instead"))

	editMsgs, _ := tr.FlattenBranch(editBranch)
	if len(editMsgs) != 2 {
		t.Errorf("edit branch messages = %d, want 2", len(editMsgs))
	}

	rewindBranch, _ := tr.Rewind(cpID)
	rewindMsgs, _ := tr.FlattenBranch(rewindBranch)
	if len(rewindMsgs) != 7 {
		t.Errorf("rewind branch messages = %d, want 7", len(rewindMsgs))
	}
}

// ===================================================================
// ToolFunc adapter
// ===================================================================

func TestToolFuncDefinitionAndExecute(t *testing.T) {
	tf := &types.ToolFunc{
		Def: types.ToolDef{
			Name:        "calculator",
			Description: "does math",
			Parameters: types.ParameterSchema{
				Type:     "object",
				Required: []string{"expression"},
				Properties: map[string]types.PropertyDef{
					"expression": {Type: "string", Description: "math expression"},
				},
			},
		},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			expr, _ := args["expression"].(string)
			return "result of " + expr, nil
		},
	}

	def := tf.Definition()
	if def.Name != "calculator" {
		t.Errorf("name = %s", def.Name)
	}
	if len(def.Parameters.Required) != 1 || def.Parameters.Required[0] != "expression" {
		t.Error("parameters wrong")
	}

	result, err := tf.Execute(context.Background(), map[string]any{"expression": "1+1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "result of 1+1" {
		t.Errorf("result = %q", result)
	}
}

// ===================================================================
// Agent with tools that receive correct arguments
// ===================================================================

func TestToolReceivesCorrectArguments(t *testing.T) {
	var receivedArgs map[string]any

	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "echo", Description: "echo args"},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			receivedArgs = args
			return "echoed", nil
		},
	}

	expectedArgs := map[string]any{
		"message": "hello",
		"count":   float64(3),
	}

	provider := &toolCallProvider{
		toolName: "echo",
		toolID:   "call-1",
		toolArgs: expectedArgs,
		response: "Done.",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("echo")})
	collectDeltas(stream)
	stream.Wait()

	if receivedArgs["message"] != "hello" {
		t.Errorf("message = %v, want 'hello'", receivedArgs["message"])
	}
	if receivedArgs["count"] != float64(3) {
		t.Errorf("count = %v, want 3", receivedArgs["count"])
	}
}

// ===================================================================
// Agent with Provider that sends multi-chunk text
// ===================================================================

func TestAgentMultiChunkTextStreaming(t *testing.T) {
	provider := &sequenceProvider{
		responses: []func(ch chan<- types.Delta){
			func(ch chan<- types.Delta) {
				ch <- types.TextStartDelta{}
				ch <- types.TextContentDelta{Content: "Hello "}
				ch <- types.TextContentDelta{Content: "beautiful "}
				ch <- types.TextContentDelta{Content: "world"}
				ch <- types.TextEndDelta{}
			},
		},
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("Hi")})
	deltas := collectDeltas(stream)
	stream.Wait()

	text := textFromDeltas(deltas)
	if text != "Hello beautiful world" {
		t.Errorf("text = %q, want 'Hello beautiful world'", text)
	}

	// Tree should have the full aggregated message
	msgs, _ := agent.Tree().FlattenBranch("main")
	am := msgs[2].(types.AssistantMessage)
	tc := am.Content[0].(types.TextContent)
	if tc.Text != "Hello beautiful world" {
		t.Errorf("tree text = %q", tc.Text)
	}
}

// ===================================================================
// Agent with mixed text + tool call in single response
// ===================================================================

func TestAgentMixedTextAndToolCallResponse(t *testing.T) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "lookup", Description: "lookup"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			return "looked up", nil
		},
	}

	provider := &sequenceProvider{
		responses: []func(ch chan<- types.Delta){
			func(ch chan<- types.Delta) {
				// Text first, then tool call
				ch <- types.TextStartDelta{}
				ch <- types.TextContentDelta{Content: "Let me search"}
				ch <- types.TextEndDelta{}
				ch <- types.ToolCallStartDelta{ID: "tc-1", Name: "lookup"}
				ch <- types.ToolCallEndDelta{Arguments: map[string]any{}}
			},
			func(ch chan<- types.Delta) {
				ch <- types.TextStartDelta{}
				ch <- types.TextContentDelta{Content: "Found it!"}
				ch <- types.TextEndDelta{}
			},
		},
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("search")})
	deltas := collectDeltas(stream)
	stream.Wait()

	// Should have both text and tool exec deltas
	texts := collectDeltasByType[types.TextContentDelta](deltas)
	execStarts := collectDeltasByType[types.ToolExecStartDelta](deltas)

	if len(texts) < 2 {
		t.Errorf("TextContentDelta count = %d, want >= 2", len(texts))
	}
	if len(execStarts) != 1 {
		t.Errorf("ToolExecStartDelta count = %d, want 1", len(execStarts))
	}

	// The assistant message in tree should have both text and tool use
	msgs, _ := agent.Tree().FlattenBranch("main")
	am := msgs[2].(types.AssistantMessage)
	if len(am.Content) != 2 {
		t.Fatalf("assistant content blocks = %d, want 2", len(am.Content))
	}
	if _, ok := am.Content[0].(types.TextContent); !ok {
		t.Error("first block should be TextContent")
	}
	if _, ok := am.Content[1].(types.ToolUseContent); !ok {
		t.Error("second block should be ToolUseContent")
	}
}

// ===================================================================
// Replay round-trip
// ===================================================================

func TestReplayRoundTrip(t *testing.T) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "greet", Description: "greet"},
		Fn:  func(_ context.Context, _ map[string]any) (string, error) { return "greeted", nil },
	}

	provider := &toolCallProvider{
		toolName: "greet",
		toolID:   "tc-1",
		toolArgs: map[string]any{},
		response: "Done greeting",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	})

	// Invoke
	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("greet me")})
	collectDeltas(stream)
	stream.Wait()

	// Get messages from tree
	msgs, _ := agent.Tree().FlattenBranch("main")

	// Replay
	replayStream := Replay(msgs)
	replayDeltas := collectDeltas(replayStream)
	replayStream.Wait()

	replayTexts := collectDeltasByType[types.TextContentDelta](replayDeltas)
	replayToolStarts := collectDeltasByType[types.ToolCallStartDelta](replayDeltas)
	replayExecStarts := collectDeltasByType[types.ToolExecStartDelta](replayDeltas)

	if len(replayTexts) != 1 || replayTexts[0].Content != "Done greeting" {
		t.Errorf("replay text = %v", replayTexts)
	}
	if len(replayToolStarts) != 1 || replayToolStarts[0].Name != "greet" {
		t.Errorf("replay tool starts = %v", replayToolStarts)
	}
	if len(replayExecStarts) != 1 {
		t.Errorf("replay exec starts = %d, want 1", len(replayExecStarts))
	}
}

// ===================================================================
// Agent with SummarizeCompactor in a multi-turn scenario
// ===================================================================

func TestAgentWithSummarizeCompactor(t *testing.T) {
	callIdx := 0
	provider := &sequenceProvider{
		responses: make([]func(ch chan<- types.Delta), 20),
	}
	for i := range provider.responses {
		i := i
		provider.responses[i] = func(ch chan<- types.Delta) {
			ch <- types.TextStartDelta{}
			ch <- types.TextContentDelta{Content: fmt.Sprintf("response-%d", i)}
			ch <- types.TextEndDelta{}
		}
	}
	_ = callIdx

	tr, _ := tree.New(types.NewSystemMessage("You are helpful."))
	agent := NewAgent(AgentConfig{
		Provider:   provider,
		CompactCfg: &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 5},
		Tree:       tr,
	})

	// Multiple turns
	for i := 0; i < 4; i++ {
		stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage(fmt.Sprintf("turn-%d", i))})
		collectDeltas(stream)
		stream.Wait()
	}

	// Tree should have all messages
	msgs, _ := tr.FlattenBranch("main")
	// sys + 4*(user + asst) = 9
	if len(msgs) != 9 {
		t.Errorf("tree messages = %d, want 9", len(msgs))
	}
}

// ===================================================================
// FlattenAnnotated with compacted nodes
// ===================================================================

func TestFlattenAnnotatedWithCompactedNodes(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	current := root
	for i := 0; i < 6; i++ {
		var msg types.Message
		if i%2 == 0 {
			msg = types.NewUserMessage(fmt.Sprintf("user-%d", i))
		} else {
			msg = types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: fmt.Sprintf("asst-%d", i)}}}
		}
		node, _ := tr.AddChild(context.Background(), current.ID, msg)
		current = node
	}

	provider := &mockProvider{response: "summary"}
	tokenizer := &mockTokenizer{tokensPerMessage: 100}

	newBranch, err := tr.Compact(context.Background(), "main", provider, tokenizer, tree.CompactOpts{
		MaxTokens: 300,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	annotated, err := tr.FlattenBranchAnnotated(newBranch)
	if err != nil {
		t.Fatalf("FlattenBranchAnnotated: %v", err)
	}

	if len(annotated) == 0 {
		t.Error("annotated should not be empty")
	}

	for i, a := range annotated {
		if a.NodeID == "" {
			t.Errorf("annotated[%d] has empty NodeID", i)
		}
	}
}

// ===================================================================
// Stress test: many concurrent operations on tree
// ===================================================================

func TestTreeConcurrentReadWrite(t *testing.T) {
	tr, _ := tree.New(types.NewSystemMessage("sys"))
	root := tr.Root()

	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	// 10 writers adding children
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := tr.AddChild(context.Background(), root.ID, types.NewUserMessage(fmt.Sprintf("writer-%d", idx)))
			if err != nil {
				errCh <- fmt.Errorf("writer %d: %w", idx, err)
			}
		}(i)
	}

	// 10 readers flattening
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tr.FlattenBranch("main")
			if err != nil {
				errCh <- err
			}
		}()
	}

	// 5 readers getting children
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tr.Children(root.ID)
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent error: %v", err)
	}

	// All writers should have succeeded
	children, _ := tr.Children(root.ID)
	if len(children) != 10 {
		t.Errorf("children = %d, want 10", len(children))
	}
}

// ===================================================================
// SlidingWindowCompactor edge cases
// ===================================================================

func TestSlidingWindowExactlyAtBoundary(t *testing.T) {
	compactor := types.NewSlidingWindowCompactor(3)

	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
		types.NewUserMessage("two"),
		types.NewUserMessage("three"),
	}

	result, _ := compactor.Compact(context.Background(), msgs, nil)
	if len(result) != 4 {
		t.Errorf("messages = %d, want 4 (no compaction at boundary)", len(result))
	}
}

func TestSlidingWindowOneOverBoundary(t *testing.T) {
	compactor := types.NewSlidingWindowCompactor(3)

	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
		types.NewUserMessage("two"),
		types.NewUserMessage("three"),
		types.NewUserMessage("four"),
	}

	result, _ := compactor.Compact(context.Background(), msgs, nil)
	if len(result) != 4 {
		t.Errorf("messages = %d, want 4", len(result))
	}
	if result[0].Role() != types.RoleSystem {
		t.Error("first should be system")
	}
}

func TestSlidingWindowPreservesSystem(t *testing.T) {
	compactor := types.NewSlidingWindowCompactor(2)

	msgs := []types.Message{
		types.NewSystemMessage("important system prompt"),
		types.NewUserMessage("old 1"),
		types.NewUserMessage("old 2"),
		types.NewUserMessage("old 3"),
		types.NewUserMessage("recent 1"),
		types.NewUserMessage("recent 2"),
	}

	result, _ := compactor.Compact(context.Background(), msgs, nil)
	if len(result) != 3 {
		t.Fatalf("messages = %d, want 3", len(result))
	}

	sm := result[0].(types.SystemMessage)
	tc := sm.Content[0].(types.TextContent)
	if tc.Text != "important system prompt" {
		t.Error("system prompt not preserved")
	}
}

// ===================================================================
// SummarizeCompactor edge cases
// ===================================================================

func TestSummarizeCompactorFewMessages(t *testing.T) {
	compactor := types.NewSummarizeCompactor(2)
	provider := &mockProvider{response: "summary"}

	msgs := []types.Message{
		types.NewSystemMessage("sys"),
		types.NewUserMessage("one"),
		types.NewUserMessage("two"),
	}

	result, err := compactor.Compact(context.Background(), msgs, provider)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if len(result) != 3 {
		t.Errorf("messages = %d, want 3 (empty toSummarize)", len(result))
	}
}

// ===================================================================
// Delta type verification
// ===================================================================

func TestAllDeltaTypesImplementInterface(t *testing.T) {
	// Verify all delta types satisfy the Delta interface (compile-time check)
	deltas := []types.Delta{
		types.TextStartDelta{},
		types.TextContentDelta{Content: "test"},
		types.TextEndDelta{},
		types.ToolCallStartDelta{ID: "1", Name: "tool"},
		types.ToolCallArgumentDelta{Content: "{}"},
		types.ToolCallEndDelta{Arguments: map[string]any{}},
		types.ToolExecStartDelta{ToolCallID: "1", Name: "tool"},
		types.ToolExecDelta{ToolCallID: "1", Inner: types.TextContentDelta{Content: "inner"}},
		types.ToolExecEndDelta{ToolCallID: "1", Result: "ok"},
		types.ErrorDelta{Error: errors.New("err")},
		types.DoneDelta{},
	}

	for i, d := range deltas {
		if d == nil {
			t.Errorf("delta[%d] is nil", i)
		}
	}
}

// ===================================================================
// Content type verification
// ===================================================================

func TestContentTypeRoleConstraints(t *testing.T) {
	// TextContent is valid in all roles
	var _ types.SystemContent = types.TextContent{Text: "hi"}
	var _ types.UserContent = types.TextContent{Text: "hi"}
	var _ types.AssistantContent = types.TextContent{Text: "hi"}

	// ToolUseContent is only for assistant
	var _ types.AssistantContent = types.ToolUseContent{ID: "1", Name: "tool"}

	// ToolResultContent is for system and user
	var _ types.SystemContent = types.ToolResultContent{ToolCallID: "1", Text: "result"}
	var _ types.UserContent = types.ToolResultContent{ToolCallID: "1", Text: "result"}
}

// ===================================================================
// NewID uniqueness
// ===================================================================

func TestNewIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := types.NewID()
		if seen[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

// ===================================================================
// Agent with nil tools creates empty registry
// ===================================================================

func TestAgentNilToolsCreatesEmptyRegistry(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "hi"},
		SystemPrompt: "sys",
		Tools:        nil,
	})

	defs := agent.tools.Definitions()
	if len(defs) != 0 {
		t.Errorf("definitions = %d, want 0", len(defs))
	}
}

// ===================================================================
// Agent with tools passed as Tools field
// ===================================================================

func TestAgentWithToolsField(t *testing.T) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "custom", Description: "custom tool"},
		Fn:  func(_ context.Context, _ map[string]any) (string, error) { return "custom result", nil },
	}

	reg := types.NewToolRegistry(tool)
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "hi"},
		SystemPrompt: "sys",
		Tools:        reg,
	})

	_, found := agent.tools.Get("custom")
	if !found {
		t.Error("custom tool not found in agent's registry")
	}
}

// ===================================================================
// SubAgent MaxIter respected
// ===================================================================

func TestSubAgentMaxIterRespected(t *testing.T) {
	// Child provider always wants to call a tool
	childProvider := &sequenceProvider{
		responses: make([]func(ch chan<- types.Delta), 100),
	}
	for i := range childProvider.responses {
		childProvider.responses[i] = func(ch chan<- types.Delta) {
			ch <- types.ToolCallStartDelta{ID: "call", Name: "child_tool"}
			ch <- types.ToolCallEndDelta{Arguments: map[string]any{}}
		}
	}

	childTool := &types.ToolFunc{
		Def: types.ToolDef{Name: "child_tool", Description: "child tool"},
		Fn:  func(_ context.Context, _ map[string]any) (string, error) { return "ok", nil },
	}

	parentProvider := &toolCallProvider{
		toolName: "delegate_to_child",
		toolID:   "parent-call",
		toolArgs: map[string]any{"task": "loop forever"},
		response: "parent done",
	}

	agent := NewAgent(AgentConfig{
		Provider:     parentProvider,
		SystemPrompt: "parent",
		SubAgents: []SubAgentDef{
			{
				Name:         "child",
				Description:  "child that loops",
				SystemPrompt: "child",
				Provider:     childProvider,
				Tools:        types.NewToolRegistry(childTool),
				MaxIter:      2, // limit child iterations
			},
		},
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})

	done := make(chan struct{})
	go func() {
		collectDeltas(stream)
		stream.Wait()
		close(done)
	}()

	select {
	case <-done:
		// good -- child respected MaxIter
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not complete -- child MaxIter not respected")
	}
}

// ===================================================================
// Feedback (RLHF)
// ===================================================================

func TestFeedback(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "Hello",
		Provider:     &mockProvider{response: "I am helpful"},
	})

	// Run a conversation.
	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("Hi"),
	})
	collectDeltas(stream)
	stream.Wait()

	// Find the assistant node to rate.
	branch := agent.Tree().Active()
	tip, err := agent.Tree().Tip(branch)
	if err != nil {
		t.Fatal(err)
	}

	// Leave positive feedback — creates a dead-end branch off the tip.
	fbNode, err := agent.Feedback(context.Background(), tip.ID, types.RatingPositive, "Great response!")
	if err != nil {
		t.Fatal(err)
	}
	if fbNode == nil {
		t.Fatal("expected feedback node")
	}
	if fbNode.State != types.NodeFeedback {
		t.Errorf("expected NodeFeedback state, got %d", fbNode.State)
	}

	// Leave a second feedback on the same node — both are siblings.
	_, err = agent.Feedback(context.Background(), tip.ID, types.RatingNegative, "Actually, not helpful")
	if err != nil {
		t.Fatal(err)
	}

	// Collect feedback summary — scans the whole tree.
	entries := agent.FeedbackSummary()
	if len(entries) != 2 {
		t.Fatalf("expected 2 feedback entries, got %d", len(entries))
	}

	// Verify both ratings are present (order is non-deterministic from map iteration).
	ratings := map[types.Rating]string{}
	for _, e := range entries {
		ratings[e.Rating] = e.Comment
		if e.TargetNodeID != tip.ID {
			t.Errorf("expected target %s, got %s", tip.ID, e.TargetNodeID)
		}
	}
	if ratings[types.RatingPositive] != "Great response!" {
		t.Errorf("missing positive feedback")
	}
	if ratings[types.RatingNegative] != "Actually, not helpful" {
		t.Errorf("missing negative feedback")
	}
}

func TestFeedbackIsPermanentLeaf(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "Hello",
		Provider:     &mockProvider{response: "response"},
	})

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("Hi"),
	})
	collectDeltas(stream)
	stream.Wait()

	tip, _ := agent.Tree().Tip(agent.Tree().Active())
	fbNode, _ := agent.Feedback(context.Background(), tip.ID, types.RatingPositive, "good")

	// Cannot add children to a feedback node.
	_, err := agent.Tree().AddChild(context.Background(), fbNode.ID, types.NewUserMessage("nope"))
	if err == nil {
		t.Fatal("expected error adding child to feedback node")
	}

	// Cannot branch from a feedback node.
	_, _, err = agent.Tree().Branch(context.Background(), fbNode.ID, "nope", types.NewUserMessage("nope"))
	if err == nil {
		t.Fatal("expected error branching from feedback node")
	}
}

func TestFeedbackNotInFlatten(t *testing.T) {
	agent := NewAgent(AgentConfig{
		Name:         "test",
		SystemPrompt: "Hello",
		Provider:     &mockProvider{response: "response"},
	})

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("Hi"),
	})
	collectDeltas(stream)
	stream.Wait()

	tip, _ := agent.Tree().Tip(agent.Tree().Active())
	agent.Feedback(context.Background(), tip.ID, types.RatingPositive, "nice")

	// Feedback is on a dead-end branch — should NOT appear in the main branch flatten.
	messages, err := agent.Tree().FlattenBranch(agent.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range messages {
		if um, ok := msg.(types.UserMessage); ok {
			for _, c := range um.Content {
				if _, ok := c.(types.FeedbackContent); ok {
					t.Fatal("feedback should not appear in main branch flatten")
				}
			}
		}
	}
}

func TestFeedbackReplay(t *testing.T) {
	messages := []types.Message{
		types.NewSystemMessage("system"),
		types.NewUserMessage("hello"),
		types.AssistantMessage{Content: []types.AssistantContent{
			types.TextContent{Text: "Hi there!"},
		}},
		types.UserMessage{Content: []types.UserContent{
			types.FeedbackContent{
				TargetNodeID: "node-123",
				Rating:       types.RatingNegative,
				Comment:      "too terse",
			},
		}},
	}

	stream := Replay(messages)
	var gotFeedback bool
	for d := range stream.Deltas() {
		if fb, ok := d.(types.FeedbackDelta); ok {
			gotFeedback = true
			if fb.Rating != types.RatingNegative {
				t.Errorf("expected negative rating, got %d", fb.Rating)
			}
			if fb.Comment != "too terse" {
				t.Errorf("expected comment 'too terse', got %q", fb.Comment)
			}
			if fb.TargetNodeID != "node-123" {
				t.Errorf("expected target node-123, got %s", fb.TargetNodeID)
			}
		}
	}
	if !gotFeedback {
		t.Fatal("expected FeedbackDelta during replay")
	}
}
