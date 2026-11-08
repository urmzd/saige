package native

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/anthropic"
	saigetypes "github.com/urmzd/saige/agent/types"
)

// anthropicBody builds the Anthropic adapter the way a Runner does, sends one
// request of a tool loop at a local server, and returns the JSON body that
// reached it. The server answers with an error: only the request matters.
func anthropicBody(t *testing.T, model, cacheTTL string) map[string]any {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"stop here"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	a, err := newAnthropic("test-key", model, cacheTTL, anthropic.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("newAnthropic: %v", err)
	}
	ch, err := a.Stream(context.Background(), saigetypes.Request{
		Tools: []saigetypes.ToolDef{
			{Name: "alpha", Description: "first", Parameters: saigetypes.ParameterSchema{Type: "object"}},
			{Name: "bravo", Description: "second", Parameters: saigetypes.ParameterSchema{Type: "object"}},
		},
		Messages: []saigetypes.Message{
			saigetypes.SystemMsg(saigetypes.Text("You are docs-bot.")),
			saigetypes.UserMsg(saigetypes.Text("first question")),
			saigetypes.AssistantMsg(saigetypes.Text("first answer")),
			saigetypes.UserMsg(saigetypes.Text("second question")),
		},
	})
	if err == nil {
		for range ch {
		}
	}
	if len(body) == 0 {
		t.Fatalf("no request reached the server (stream error: %v)", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, body)
	}
	return out
}

// marked reports the cache lifetime on a request block, or "" for none.
func marked(block any) string {
	m, _ := block.(map[string]any)
	cc, _ := m["cache_control"].(map[string]any)
	ttl, _ := cc["ttl"].(string)
	return ttl
}

func blocks(t *testing.T, v any, what string) []any {
	t.Helper()
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("request has no %s: %v", what, v)
	}
	return list
}

// Cache breakpoints only pay off at the end of a stable prefix. They belong on
// the last tool, the last system block, and the newest message, and nowhere
// earlier: a breakpoint on an early message would spend one of the API's four
// on a prefix the later ones already cover.
func TestAnthropicRequestsMarkTheEndOfEachStablePrefix(t *testing.T) {
	t.Parallel()

	for _, ttl := range []string{PromptCacheShort, PromptCacheLong} {
		body := anthropicBody(t, "claude-sonnet-4-6", ttl)

		tools := blocks(t, body["tools"], "tools")
		for i, tool := range tools {
			want := ""
			if i == len(tools)-1 {
				want = ttl
			}
			if got := marked(tool); got != want {
				t.Errorf("ttl %s: tool %d cache marker = %q, want %q", ttl, i, got, want)
			}
		}

		system := blocks(t, body["system"], "system blocks")
		if got := marked(system[len(system)-1]); got != ttl {
			t.Errorf("ttl %s: last system block cache marker = %q, want %q", ttl, got, ttl)
		}

		msgs := blocks(t, body["messages"], "messages")
		for i, m := range msgs {
			content := blocks(t, m.(map[string]any)["content"], "message content")
			want := ""
			if i == len(msgs)-1 {
				want = ttl
			}
			if got := marked(content[len(content)-1]); got != want {
				t.Errorf("ttl %s: message %d cache marker = %q, want %q", ttl, i, got, want)
			}
		}
	}
}

// Turning the cache off must send a request with no markers at all, so a
// deployment that does not want to pay the cache-write premium can opt out.
func TestRequestsCarryNoCacheMarkersWhenCachingIsOff(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(anthropicBody(t, "claude-sonnet-4-6", PromptCacheOff))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "cache_control") {
		t.Errorf("request carries a cache marker: %s", raw)
	}
	if !strings.Contains(string(raw), "second question") {
		t.Errorf("request did not carry the conversation: %s", raw)
	}
}

// A reasoning effort the model takes is sent with every request; one it does
// not take is refused before any request, naming what the model takes.
func TestAnthropicEffortIsSentOrRefused(t *testing.T) {
	t.Parallel()

	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"stop here"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	const model = "claude-opus-4-6"
	a, err := newAnthropic("test-key", model, PromptCacheOff, anthropic.WithBaseURL(srv.URL), anthropic.WithReasoningEffort("low"))
	if err != nil {
		t.Fatalf("newAnthropic: %v", err)
	}
	caps := a.Capabilities()
	if len(caps.ReasoningEfforts) == 0 {
		t.Skipf("saige's catalog lists no efforts for %s", model)
	}
	if err := checkEffort(Spec{Name: "a", Provider: "anthropic", Model: model, Effort: "low"}, caps); err != nil {
		t.Fatalf("checkEffort refused an effort the model takes: %v", err)
	}
	ch, err := a.Stream(context.Background(), saigetypes.Request{
		Messages: []saigetypes.Message{saigetypes.UserMsg(saigetypes.Text("hi"))},
	})
	if err == nil {
		for range ch {
		}
	}
	var out struct {
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, body)
	}
	if out.OutputConfig.Effort != "low" {
		t.Errorf("request sent effort %q, want low\n%s", out.OutputConfig.Effort, body)
	}

	err = checkEffort(Spec{Name: "a", Provider: "anthropic", Model: model, Effort: "ludicrous"}, caps)
	if err == nil || !strings.Contains(err.Error(), "ludicrous") {
		t.Errorf("checkEffort accepted an effort the model does not take: %v", err)
	}
	if err := checkEffort(Spec{Name: "a", Effort: "low"}, saigetypes.ModelCapabilities{}); err == nil {
		t.Error("checkEffort accepted an effort for a model that takes none")
	}
}
