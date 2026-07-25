package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// captureChat starts a server that records the decoded chat request and
// replies with the given newline-delimited chunks.
func captureChat(t *testing.T, chunks ...ChatChunk) (*httptest.Server, *ChatRequest) {
	t.Helper()
	var got ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, c := range chunks {
			if err := json.NewEncoder(w).Encode(c); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, &got
}

func drain(rx <-chan ChatChunk) {
	for range rx {
	}
}

func TestChatStreamSendsOptionsAndThink(t *testing.T) {
	server, got := captureChat(t, ChatChunk{Done: true})

	client := NewClient(server.URL, "test-model", "",
		WithChatOptions(Options{NumCtx: 16384, Temperature: 0.4}),
		WithThink(false),
	)
	rx, err := client.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	drain(rx)

	opts, ok := got.Options.(map[string]any)
	if !ok {
		t.Fatalf("options not sent as an object: %#v", got.Options)
	}
	if opts["num_ctx"] != float64(16384) {
		t.Errorf("num_ctx = %v, want 16384", opts["num_ctx"])
	}
	if opts["temperature"] != 0.4 {
		t.Errorf("temperature = %v, want 0.4", opts["temperature"])
	}
	if got.Think == nil || *got.Think {
		t.Errorf("think = %v, want false", got.Think)
	}
}

func TestChatStreamOmitsOptionsWhenUnset(t *testing.T) {
	server, got := captureChat(t, ChatChunk{Done: true})

	client := NewClient(server.URL, "test-model", "")
	rx, err := client.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	drain(rx)

	// An unconfigured client must not override the daemon's own defaults.
	if got.Options != nil {
		t.Errorf("options = %#v, want nil", got.Options)
	}
	if got.Think != nil {
		t.Errorf("think = %v, want nil", *got.Think)
	}
}

func TestChatStreamWithFormatDisablesThinkingByDefault(t *testing.T) {
	server, got := captureChat(t, ChatChunk{Done: true})

	client := NewClient(server.URL, "test-model", "")
	rx, err := client.ChatStreamWithFormat(context.Background(),
		[]ChatMessage{{Role: "user", Content: "hi"}}, nil,
		map[string]any{"type": "object"})
	if err != nil {
		t.Fatalf("ChatStreamWithFormat: %v", err)
	}
	drain(rx)

	// Grammar-constrained output and chain-of-thought fight each other, so a
	// caller who asked for a schema and said nothing about thinking gets it off.
	if got.Think == nil {
		t.Fatal("think was not set; schema-constrained requests must disable it")
	}
	if *got.Think {
		t.Error("think = true, want false")
	}
}

func TestChatStreamWithFormatRespectsExplicitThink(t *testing.T) {
	server, got := captureChat(t, ChatChunk{Done: true})

	client := NewClient(server.URL, "test-model", "", WithThink(true))
	rx, err := client.ChatStreamWithFormat(context.Background(),
		[]ChatMessage{{Role: "user", Content: "hi"}}, nil,
		map[string]any{"type": "object"})
	if err != nil {
		t.Fatalf("ChatStreamWithFormat: %v", err)
	}
	drain(rx)

	if got.Think == nil || !*got.Think {
		t.Errorf("think = %v, want true (explicit setting must win)", got.Think)
	}
}

func TestChatStreamErrorIncludesResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":"model 'ghost' not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(server.URL, "ghost", "")
	_, err := client.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// A bare status code leaves the caller guessing; the body says what to fix.
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not carry the response body", err)
	}
}

func TestAdapterTranslatesThinkingDeltas(t *testing.T) {
	server, _ := captureChat(t,
		ChatChunk{Message: ChatMessage{Role: "assistant", Thinking: "let me "}},
		ChatChunk{Message: ChatMessage{Role: "assistant", Thinking: "consider"}},
		ChatChunk{Message: ChatMessage{Role: "assistant", Content: "answer"}},
		ChatChunk{Done: true, DoneReason: "stop"},
	)

	adapter := NewAdapter(NewClient(server.URL, "test-model", ""))
	rx, err := adapter.ChatStream(context.Background(),
		[]types.Message{types.NewUserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var kinds []string
	var thinking, text strings.Builder
	for d := range rx {
		switch v := d.(type) {
		case types.ThinkingStartDelta:
			kinds = append(kinds, "think-start")
		case types.ThinkingContentDelta:
			kinds = append(kinds, "think-content")
			thinking.WriteString(v.Content)
		case types.ThinkingEndDelta:
			kinds = append(kinds, "think-end")
		case types.TextStartDelta:
			kinds = append(kinds, "text-start")
		case types.TextContentDelta:
			kinds = append(kinds, "text-content")
			text.WriteString(v.Content)
		case types.TextEndDelta:
			kinds = append(kinds, "text-end")
		}
	}

	if thinking.String() != "let me consider" {
		t.Errorf("thinking = %q, want %q", thinking.String(), "let me consider")
	}
	if text.String() != "answer" {
		t.Errorf("text = %q, want %q", text.String(), "answer")
	}

	// Thinking must close before text opens, so consumers can rely on the
	// blocks not interleaving.
	order := strings.Join(kinds, ",")
	want := "think-start,think-content,think-content,think-end,text-start,text-content,text-end"
	if order != want {
		t.Errorf("delta order =\n  %s\nwant\n  %s", order, want)
	}
}
