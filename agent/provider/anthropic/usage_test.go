package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestRepeatedUsageSnapshotsAreNotDoubleCounted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []struct{ kind, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":60,"cache_read_input_tokens":30,"cache_creation_input_tokens":10,"output_tokens":2}}}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.kind, event.data)
		}
	}))
	defer server.Close()
	adapter := NewAdapter("test", "claude-sonnet-4-5", WithBaseURL(server.URL))
	stream, err := adapter.ChatStream(context.Background(), []types.Message{types.NewUserMessage("go")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var usage types.UsageDelta
	for delta := range stream {
		switch d := delta.(type) {
		case types.UsageDelta:
			usage = usage.Merge(d)
		case types.ErrorDelta:
			t.Fatal(d.Error)
		}
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 12 || usage.TotalTokens != 112 || usage.CachedPromptTokens != 30 || usage.CacheWriteTokens != 10 {
		t.Fatalf("%+v", usage)
	}
}
