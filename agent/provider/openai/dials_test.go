package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func highDepth() types.DialLayer {
	return types.DialLayer{Scope: types.DialScopeEntry, Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}}
}

// Scenario (d) on Chat Completions: a model that takes tools there only
// without reasoning is sent effort none when tools are offered, and the
// configured depth otherwise.
func TestDialsCompileForChatSurface(t *testing.T) {
	server, bodies := captureServer(t)
	a := NewAdapter("k", "gpt-6-luna", WithBaseURL(server.URL), WithDials(highDepth()))
	for _, tools := range [][]types.ToolDef{testTools, nil} {
		ch, err := a.ChatStream(context.Background(), []types.Message{types.NewUserMessage("hi")}, tools)
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}
	if got := (*bodies)[0]["reasoning_effort"]; got != "none" {
		t.Fatalf("with tools: reasoning_effort = %v", got)
	}
	if got := (*bodies)[1]["reasoning_effort"]; got != "high" {
		t.Fatalf("without tools: reasoning_effort = %v", got)
	}
}

// Scenario (d) on the Responses API: the effort stays with tools.
func TestDialsCompileForResponsesSurface(t *testing.T) {
	var body atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		body.Store(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textDelta("ok") + completed))
	}))
	t.Cleanup(server.Close)
	a := NewResponsesAdapter("k", "gpt-6-luna", WithBaseURL(server.URL))
	focused := types.CreativityFocused
	ch, err := a.ChatStreamWithOptions(context.Background(), []types.Message{types.NewUserMessage("hi")}, testTools,
		types.RequestOptions{Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}, Creativity: &focused}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	var got map[string]any
	_ = json.Unmarshal(body.Load().([]byte), &got)
	if r, _ := got["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Fatalf("reasoning = %v", got["reasoning"])
	}
	if _, ok := got["temperature"]; ok {
		t.Fatalf("creativity must yield to reasoning: %v", got)
	}
}

// A raw option keeps strict rejection: temperature with active reasoning
// fails before the request, dials or not.
func TestRawOptionStaysStrictBesideDials(t *testing.T) {
	server, bodies := captureServer(t)
	a := NewAdapter("k", "gpt-6-luna", WithBaseURL(server.URL))
	temp := 0.2
	_, err := a.ChatStreamWithOptions(context.Background(), []types.Message{types.NewUserMessage("hi")}, nil,
		types.RequestOptions{Temperature: &temp, Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}})
	if !errors.Is(err, types.ErrInvalidModelConfig) || len(*bodies) != 0 {
		t.Fatalf("err = %v, requests = %d", err, len(*bodies))
	}
}
