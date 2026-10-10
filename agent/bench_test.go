package agent

import (
	"context"
	"strconv"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// BenchmarkAgentTextLoop measures the per-turn overhead of the agent loop for a
// plain text response (no tools), using an in-memory mock provider so the number
// reflects SDK overhead, not network latency.
func BenchmarkAgentTextLoop(b *testing.B) {
	provider := &mockProvider{response: "the answer is 42"}
	input := []types.Message{types.UserMsg(types.Text("what is the answer?"))}

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
	input := []types.Message{types.UserMsg(types.Text("use the tool"))}

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
	input := []types.Message{types.UserMsg(types.Text("go"))}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := NewAgent(AgentConfig{Provider: provider, SystemPrompt: "s"})
		_, _ = a.RunDurable(context.Background(), types.NoopStepRunner{}, input, "")
	}
}

// parallelToolProvider asks for parallelToolCalls echo calls on a
// conversation's first turn and answers once their results are in. It keeps
// no state, so one instance serves any number of concurrent runs.
type parallelToolProvider struct{}

const parallelToolCalls = 8

func (parallelToolProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	messages := req.Messages
	ch := make(chan types.Delta, 2*parallelToolCalls+3)
	if hasToolResult(messages) {
		ch <- types.PartStart{Index: 0, Kind: types.KindText}
		ch <- types.PartDelta{Index: 0, Text: "done"}
		ch <- types.PartEnd{Index: 0}
	} else {
		for i := range parallelToolCalls {
			id := "c" + strconv.Itoa(i)
			ch <- types.PartStart{Index: i, Kind: types.KindToolCall, ID: id, Name: "echo"}
			ch <- types.PartEnd{Index: i, Part: types.ToolCallPart{ID: id, Name: "echo", Arguments: map[string]any{}}}
		}
	}
	close(ch)
	return ch, nil
}

// BenchmarkAgentParallelTools measures a turn with parallel tool calls
// followed by a text turn, with runs on every P. Compare -cpu 1,4,8 to see
// how the loop scales across cores.
func BenchmarkAgentParallelTools(b *testing.B) {
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "echo", Description: "echo"},
		Fn:  func(context.Context, map[string]any) (string, error) { return "ok", nil },
	}
	tools := types.NewToolRegistry(tool)
	input := []types.Message{types.UserMsg(types.Text("use the tools"))}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			a := NewAgent(AgentConfig{Provider: parallelToolProvider{}, Tools: tools, SystemPrompt: "s"})
			stream := a.Invoke(context.Background(), input)
			for range stream.Deltas() {
			}
			if err := stream.Wait(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
