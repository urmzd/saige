package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestClientLogsOnlyWhenAsked checks that a default client writes nothing to
// the standard logger, and that WithLogger traces calls.
func TestClientLogsOnlyWhenAsked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GenerateResponse{Response: "ok", Done: true, DoneReason: "stop"})
	}))
	defer server.Close()

	var std bytes.Buffer
	log.SetOutput(&std)
	defer log.SetOutput(os.Stderr)

	c := NewClient(server.URL, "qwen3:4b", "")
	if c.Logger != nil {
		t.Fatal("a default client must not have a logger")
	}
	if _, err := c.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if std.Len() != 0 {
		t.Fatalf("default client logged: %q", std.String())
	}

	var traced bytes.Buffer
	c = NewClient(server.URL, "qwen3:4b", "", WithLogger(log.New(&traced, "", 0)))
	if _, err := c.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(traced.String(), "[ollama] generate") {
		t.Fatalf("WithLogger did not trace the call: %q", traced.String())
	}
}
