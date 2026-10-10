package preset_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

// capture answers every request with a permanent 400 and keeps its body by
// host, so each attempt's wire request can be inspected without a network.
type capture struct {
	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	c.mu.Lock()
	c.bodies[r.URL.Host] = append(c.bodies[r.URL.Host], body)
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
		Body:    io.NopCloser(bytes.NewReader([]byte(`{"error":{"type":"invalid_request_error","message":"captured","code":400,"status":"INVALID_ARGUMENT"}}`))),
		Request: r}, nil
}

func TestWireConsistency(t *testing.T) {
	cat := overlay(t, `{"version":1,"presets":{"w":{
		"options":{"max_output_tokens":1500},
		"retry":{"disable":true},
		"chain":[
		 {"id":"claude","provider":"anthropic","model":"claude-sonnet-4-6","options":{"reasoning":{"budget":1024},
		   "prompt_cache":{"mode":"markers","ttl":"5m","system":true}}},
		 {"id":"haiku","provider":"anthropic","model":"claude-3-5-haiku","options":{"temperature":0.2}},
		 {"id":"gpt","provider":"openai","model":"gpt-4.1","options":{"temperature":0.3,"seed":7}},
		 {"id":"o3","provider":"openai","model":"o3","options":{"reasoning":{"effort":"high"}},"unset":["max_output_tokens"]},
		 {"id":"gemini","provider":"google","model":"gemini-2.5-flash","options":{"temperature":0.5,"seed":9,"reasoning":{"budget":512}}}]}}}`)
	wire := &capture{bodies: map[string][]map[string]any{}}
	b, err := preset.Build(context.Background(), cat, "w", nil, preset.Options{Getenv: everyone, HTTPClient: &http.Client{Transport: wire}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(context.Background()) }()
	sess := b.Session().(types.TargetSwitcher)
	for _, id := range []types.ProfileID{"w/claude", "w/haiku", "w/gpt", "w/o3", "w/gemini"} {
		pinned, err := sess.WithTarget(types.ProfileTarget(id))
		if err != nil {
			t.Fatal(err)
		}
		ch, err := pinned.Stream(context.Background(), types.Request{Messages: []types.Message{
			types.SystemMsg(types.Text("sys")), types.UserMsg(types.Text("hi"))}})
		if err == nil {
			for range ch {
			}
		}
	}
	anth := wire.bodies["api.anthropic.com"]
	oai := wire.bodies["api.openai.com"]
	goog := wire.bodies["generativelanguage.googleapis.com"]
	if len(anth) != 2 || len(oai) != 2 || len(goog) != 1 {
		t.Fatalf("requests: anthropic %d openai %d google %d", len(anth), len(oai), len(goog))
	}
	claude, haiku := anth[0], anth[1]
	if claude["max_tokens"] != 1500.0 || claude["temperature"] != nil {
		t.Fatalf("claude body: %v", claude)
	}
	if th, _ := claude["thinking"].(map[string]any); th["budget_tokens"] != 1024.0 {
		t.Fatalf("claude thinking: %v", claude["thinking"])
	}
	if sys, _ := json.Marshal(claude["system"]); !strings.Contains(string(sys), "cache_control") {
		t.Fatalf("claude cache markers: %s", sys)
	}
	if haiku["temperature"] != 0.2 || haiku["thinking"] != nil {
		t.Fatalf("haiku body: %v", haiku)
	}
	if sys, _ := json.Marshal(haiku["system"]); strings.Contains(string(sys), "cache_control") {
		t.Fatal("haiku inherited another entry's cache markers")
	}
	gpt, o3 := oai[0], oai[1]
	if gpt["temperature"] != 0.3 || gpt["seed"] != 7.0 || gpt["max_completion_tokens"] != 1500.0 || gpt["reasoning_effort"] != nil {
		t.Fatalf("gpt body: %v", gpt)
	}
	if o3["reasoning_effort"] != "high" || o3["temperature"] != nil || o3["seed"] != nil || o3["max_completion_tokens"] != nil {
		t.Fatalf("o3 body: %v", o3)
	}
	gen, _ := goog[0]["generationConfig"].(map[string]any)
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if gen["temperature"] != 0.5 || gen["seed"] != 9.0 || gen["maxOutputTokens"] != 1500.0 || tc["thinkingBudget"] != 512.0 {
		t.Fatalf("gemini body: %v", goog[0])
	}
}
