package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func captureServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunk(`"delta":{"content":"ok"}`)+finish("stop")+usageChunk+"data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

var testTools = []types.ToolDef{
	{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}},
	{Name: "write", Parameters: types.ParameterSchema{Type: "object"}},
}

func TestToolChoiceWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		choice  *types.ToolChoice
		tools   []types.ToolDef
		want    string // fmt.Sprint of tool_choice; "" means absent
		wantErr bool
	}{
		{name: "unset", tools: testTools},
		{name: "auto", choice: &types.ToolChoice{Mode: types.ToolChoiceAuto}, tools: testTools, want: "auto"},
		{name: "none", choice: &types.ToolChoice{Mode: types.ToolChoiceNone}, tools: testTools, want: "none"},
		{name: "required", choice: &types.ToolChoice{Mode: types.ToolChoiceRequired}, tools: testTools, want: "required"},
		{name: "named", choice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "write"}, tools: testTools,
			want: fmt.Sprint(map[string]any{"type": "function", "function": map[string]any{"name": "write"}})},
		{name: "no tools offered", choice: &types.ToolChoice{Mode: types.ToolChoiceRequired}},
		{name: "named tool not offered", choice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"}, tools: testTools, wantErr: true},
		{name: "name without named mode", choice: &types.ToolChoice{Mode: types.ToolChoiceRequired, Name: "write"}, tools: testTools, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			opts := []Option{WithBaseURL(server.URL)}
			if tc.choice != nil {
				opts = append(opts, WithToolChoice(*tc.choice))
			}
			ch, err := NewAdapter("k", testModel, opts...).Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: tc.tools})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) || len(*bodies) != 0 {
					t.Fatalf("err = %v, requests = %d; want a local configuration error", err, len(*bodies))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			got, ok := (*bodies)[0]["tool_choice"]
			if tc.want == "" {
				if ok {
					t.Fatalf("tool_choice = %v, want absent", got)
				}
				return
			}
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("tool_choice = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolChoiceNeedsCapability(t *testing.T) {
	// Non-auto modes require CapToolChoice: chat rows declare it, an
	// embedding row does not.
	for _, tc := range []struct {
		mode types.ToolChoiceMode
		ok   bool
	}{
		{types.ToolChoiceAuto, true},
		{types.ToolChoiceRequired, true},
		{types.ToolChoiceNone, true},
	} {
		if err := NewAdapter("k", testModel, WithToolChoice(types.ToolChoice{Mode: tc.mode})).Validate(); (err == nil) != tc.ok {
			t.Errorf("mode %s: Validate = %v, want ok=%v", tc.mode, err, tc.ok)
		}
	}
	if err := NewAdapter("k", "text-embedding-3-small", WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})).Validate(); err == nil {
		t.Error("an embedding model does not declare tool choice")
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o","object":"model","created":1715367049,"owned_by":"system"},{"id":"o3","object":"model","created":0,"owned_by":"system"}]}`)
	}))
	t.Cleanup(server.Close)
	models, err := NewAdapter("k", testModel, WithBaseURL(server.URL)).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gpt-4o" || models[0].Created.Unix() != 1715367049 || !models[1].Created.IsZero() {
		t.Fatalf("models = %+v", models)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)
	if _, err := NewAdapter("k", testModel, WithBaseURL(failing.URL)).ListModels(context.Background()); !types.IsAuth(err) {
		t.Fatalf("err = %v, want an auth error", err)
	}
}
