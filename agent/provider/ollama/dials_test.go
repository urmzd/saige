package ollama

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// Scenario (c) on Ollama: a depth collapses to the think flag, and the
// configured options, num_ctx included, are kept.
func TestDialsCompileToThinkFlag(t *testing.T) {
	server, got := captureChat(t, ChatChunk{Done: true})
	a := NewAdapter(NewClient(server.URL, "qwen3.5:4b", "", WithChatOptions(map[string]any{"num_ctx": 8192})))
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Options: &types.RequestOptions{Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if got.Think == nil || !*got.Think {
		t.Fatalf("think = %v", got.Think)
	}
	if opts, _ := got.Options.(map[string]any); opts["num_ctx"] != float64(8192) {
		t.Fatalf("configured options lost: %v", got.Options)
	}
	if a.Client.Think != nil {
		t.Fatal("a per-request dial changed the shared client")
	}
}

func TestOllamaRawOptionsStillRejected(t *testing.T) {
	server, _ := captureChat(t, ChatChunk{Done: true})
	temp := 0.1
	_, err := NewAdapter(NewClient(server.URL, "qwen3", "")).Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Options: &types.RequestOptions{Temperature: &temp}})
	if err == nil {
		t.Fatal("a raw temperature per request was accepted")
	}
}
