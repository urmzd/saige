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
			a := NewAdapter("test", tc.model, tc.opts...)
			err := a.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate=%v, valid=%v", err, tc.valid)
			}
			if !tc.valid {
				for _, schema := range []*types.ParameterSchema{nil, {Type: "object"}} {
					ch, err := a.ChatStreamWithSchema(context.Background(), nil, nil, schema)
					if ch != nil || !errors.Is(err, types.ErrInvalidModelConfig) {
						t.Fatalf("channel=%v error=%v", ch, err)
					}
				}
			}
		})
	}
}

func TestAdaptiveThinkingEncodingAndManualSchemaConflict(t *testing.T) {
	a := NewAdapter("test", "claude-opus-4-6", WithReasoningEffort("high"))
	var params sdk.MessageNewParams
	a.applyParams(&params)
	if params.Thinking.OfAdaptive == nil || string(params.OutputConfig.Effort) != "high" {
		t.Fatalf("bad adaptive config: %+v", params)
	}
	manual := NewAdapter("test", "claude-sonnet-4-5", WithThinking(1024))
	_, err := manual.ChatStreamWithSchema(context.Background(), nil, nil, &types.ParameterSchema{Type: "object"})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("forced schema while thinking: %v", err)
	}
	if err := a.WithModel("claude-3-5-sonnet").(*Adapter).Validate(); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("model switch: %v", err)
	}
}

func TestAdaptiveSchemaPreservesPromptCache(t *testing.T) {
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
			a := NewAdapter("test", model, WithBaseURL(server.URL), WithReasoningEffort("max"), WithSystemPromptCache("1h"))
			stream, err := a.ChatStreamWithSchema(context.Background(), []types.Message{types.NewSystemMessage("rules"), types.NewUserMessage("reply")}, nil, &types.ParameterSchema{Type: "object"})
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
			if body["tool_choice"].(map[string]any)["name"] != "structured_output" {
				t.Fatal("schema tool lost", body)
			}
			system := body["system"].([]any)[0].(map[string]any)
			if system["cache_control"].(map[string]any)["ttl"] != "1h" {
				t.Fatal("prompt cache lost", body)
			}
		})
	}
}
