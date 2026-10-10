package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestThinkingValidation(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		opts        []Option
		valid       bool
	}{
		{"manual", "claude-sonnet-4-5", []Option{WithThinking(1024)}, true},
		{"below minimum", "claude-sonnet-4-5", []Option{WithThinking(1023)}, false},
		{"budget consumes all output", "claude-sonnet-4-5", []Option{WithThinking(4096)}, false},
		{"unsupported", "claude-3-5-sonnet", []Option{WithThinking(1024)}, false},
		{"temperature conflict", "claude-sonnet-4-5", []Option{WithThinking(1024), WithTemperature(0)}, false},
		{"default temperature", "claude-sonnet-4-5", []Option{WithThinking(1024), WithTemperature(1)}, true},
		{"top p allowed", "claude-sonnet-4-5", []Option{WithThinking(1024), WithTopP(.95)}, true},
		{"top k conflict", "claude-sonnet-4-5", []Option{WithThinking(1024), WithTopK(5)}, false},
		{"adaptive", "claude-opus-4-6", []Option{WithReasoningEffort("high")}, true},
		{"adaptive only rejects budget", "claude-fable-5", []Option{WithThinking(1024)}, false},
		{"adaptive only rejects temperature", "claude-fable-5", []Option{WithTemperature(0)}, false},
		{"two modes", "claude-opus-4-6", []Option{WithThinking(1024), WithReasoningEffort("high")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := must.Get(New(Config{APIKey: "test", Model: types.ModelID(tc.model)}, tc.opts...))
			err := a.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate=%v, valid=%v", err, tc.valid)
			}
			if !tc.valid {
				for _, schema := range []*types.ParameterSchema{nil, {Type: "object"}} {
					ch, err := a.Stream(context.Background(), types.Request{Schema: schema})
					if ch != nil || !errors.Is(err, types.ErrInvalidModelConfig) {
						t.Fatalf("channel=%v error=%v", ch, err)
					}
				}
			}
		})
	}
}

func TestAdaptiveThinkingEncodingAndManualSchemaConflict(t *testing.T) {
	a := must.Get(New(Config{APIKey: "test", Model: "claude-opus-4-6"}, WithReasoningEffort("high")))
	var params sdk.MessageNewParams
	a.applyParams(&params)
	if params.Thinking.OfAdaptive == nil || string(params.OutputConfig.Effort) != "high" {
		t.Fatalf("bad adaptive config: %+v", params)
	}
	manual := must.Get(New(Config{APIKey: "test", Model: "claude-sonnet-4-5"}, WithThinking(1024)))
	_, err := manual.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("forced schema while thinking: %v", err)
	}
	if err := must.Get(a.WithTarget(types.ModelTarget("claude-3-5-sonnet"))).(*Adapter).Validate(); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("model switch: %v", err)
	}
}

func TestAdaptivePromptCache(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "claude-fable-5"} {
		t.Run(model, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			}))
			defer server.Close()
			a := must.Get(New(Config{APIKey: "test", Model: types.ModelID(model)},
				WithBaseURL(server.URL), WithReasoningEffort("max"), WithSystemPromptCache("1h")))
			stream, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.SystemMsg(types.Text("rules")), types.UserMsg(types.Text("reply"))}})
			if err != nil {
				t.Fatal(err)
			}
			for delta := range stream {
				if e, ok := delta.(types.ErrorDelta); ok {
					t.Fatal(e.Error)
				}
			}
			if body["thinking"].(map[string]any)["type"] != "adaptive" || body["output_config"].(map[string]any)["effort"] != "max" {
				t.Fatal("adaptive settings lost", body)
			}
			system := body["system"].([]any)[0].(map[string]any)
			if system["cache_control"].(map[string]any)["ttl"] != "1h" {
				t.Fatal("prompt cache lost", body)
			}
		})
	}
}

// TestSchemaWithThinkingIsRejected checks that a schema request is refused
// before any network call where the API rejects the forced hidden tool with
// a manual thinking budget. Adaptive thinking accepts a forced tool, so it
// keeps schema output, and models that reject forcing outright send the
// schema as output_config.format instead. Capabilities must agree.
func TestSchemaWithThinkingIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		opts        []Option
		reject      bool
		native      bool
	}{
		{"manual thinking", "claude-sonnet-4-5", []Option{WithThinking(1024)}, true, false},
		{"adaptive thinking", "claude-opus-4-6", []Option{WithReasoningEffort("high")}, false, false},
		{"adaptive thinking by default", "claude-haiku-5-5", nil, false, false},
		{"adaptive effort on haiku", "claude-haiku-5-5", []Option{WithReasoningEffort("low")}, false, false},
		{"model rejects forcing: sonnet", "claude-sonnet-5-5", nil, false, true},
		{"model rejects forcing: opus", "claude-opus-5-5", []Option{WithReasoningEffort("low")}, false, true},
		{"model rejects forcing: fable", "claude-fable-5-1", nil, false, true},
		{"model rejects forcing: mythos", "claude-mythos-5-1", nil, false, true},
		{"no thinking", "claude-sonnet-4-5", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			}))
			defer server.Close()
			a := must.Get(New(Config{APIKey: "test", Model: types.ModelID(tc.model)}, append([]Option{WithBaseURL(server.URL)}, tc.opts...)...))
			if got := a.Capabilities().Supports(types.CapStructuredOutput); got == tc.reject {
				t.Errorf("structured output capability = %v, want %v", got, !tc.reject)
			}
			if got := a.Capabilities().StructuredOutput != types.StructuredOutputNone; got == tc.reject {
				t.Errorf("structured output mode declared = %v, want %v", got, !tc.reject)
			}
			stream, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("reply"))}, Schema: &types.ParameterSchema{Type: "object"}})
			if tc.reject {
				if stream != nil || !errors.Is(err, types.ErrSchemaUnsupported) || !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("stream = %v, err = %v; want a schema-unsupported error", stream, err)
				}
				if body != nil {
					t.Fatal("request was sent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range stream {
			}
			if tc.native {
				format, _ := body["output_config"].(map[string]any)["format"].(map[string]any)
				schema, _ := format["schema"].(map[string]any)
				if format["type"] != "json_schema" || schema["additionalProperties"] != false {
					t.Fatal("native schema lost", body)
				}
				if body["tool_choice"] != nil || body["tools"] != nil {
					t.Fatal("native schema output must not force a tool", body)
				}
				return
			}
			if body["tool_choice"].(map[string]any)["name"] != "structured_output" {
				t.Fatal("schema tool lost", body)
			}
		})
	}
}
