package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateSingleTurnText(t *testing.T) {
	chunks := []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"hello "}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"world"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := NewAdapter("test-key", "gpt-test", WithBaseURL(server.URL))
	got, err := adapter.Generate(context.Background(), "say hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("Generate = %q, want 'hello world'", got)
	}
}

func TestGenerateSurfacesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"nope"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	adapter := NewAdapter("bad-key", "gpt-test", WithBaseURL(server.URL))
	if _, err := adapter.Generate(context.Background(), "hi"); err == nil {
		t.Fatal("expected an error from a 401 response")
	}
}
