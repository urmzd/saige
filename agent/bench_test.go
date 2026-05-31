package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// BenchmarkAgentTextLoop measures the per-turn overhead of the agent loop for a
// plain text response (no tools), using an in-memory mock provider so the number
// reflects SDK overhead, not network latency.
func BenchmarkAgentTextLoop(b *testing.B) {
	provider := &mockProvider{response: "the answer is 42"}
	input := []types.Message{types.NewUserMessage("what is the answer?")}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := NewAgent(AgentConfig{Provider: provider, SystemPrompt: "you are concise"})
		stream := a.Invoke(context.Background(), input)
		for range stream.Deltas() {
		}
		_ = stream.Wait()
	}
}

// BenchmarkAgentToolLoop measures a full tool round-trip: the model emits one
// tool call, the SDK executes it, then the model returns text.
func BenchmarkAgentToolLoop(b *testing.B) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "echo", Description: "echo"},
		Fn:  func(context.Context, map[string]any) (string, error) { return "ok", nil },
	}
	input := []types.Message{types.NewUserMessage("use the tool")}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh provider+agent so the tool-then-text script restarts each run.
		provider := &toolCallProvider{toolName: "echo", toolID: "t1", toolArgs: map[string]any{}, response: "done"}
		a := NewAgent(AgentConfig{Provider: provider, Tools: types.NewToolRegistry(tool), SystemPrompt: "s"})
		stream := a.Invoke(context.Background(), input)
		for range stream.Deltas() {
		}
		_ = stream.Wait()
	}
}

// BenchmarkRunDurableNoop measures the durable run path under the default no-op
// step runner (the overhead the durable seam adds when not memoizing).
func BenchmarkRunDurableNoop(b *testing.B) {
	provider := &mockProvider{response: "durable answer"}
	input := []types.Message{types.NewUserMessage("go")}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := NewAgent(AgentConfig{Provider: provider, SystemPrompt: "s"})
		_, _ = a.RunDurable(context.Background(), types.NoopStepRunner{}, input, "")
	}
}
