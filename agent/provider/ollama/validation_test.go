package ollama

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestAdapterValidatesExplicitReasoningAndWireOptions(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		opts        []Option
		valid       bool
	}{
		{"supported", "qwen3", []Option{WithThink(true)}, true},
		{"unsupported", "llama3.1", []Option{WithThink(true)}, false},
		{"explicit false unsupported", "unknown", []Option{WithThink(false)}, false},
		{"zero temperature map", "qwen3", []Option{WithChatOptions(map[string]any{"temperature": 0})}, true},
		{"negative temperature map", "qwen3", []Option{WithChatOptions(map[string]any{"temperature": -1})}, false},
		{"bad option type", "qwen3", []Option{WithChatOptions(map[string]any{"temperature": "cold"})}, false},
		{"top k", "qwen3", []Option{WithChatOptions(map[string]any{"top_k": 40})}, true},
		{"top k zero", "qwen3", []Option{WithChatOptions(map[string]any{"top_k": 0})}, true},
		{"top k negative", "qwen3", []Option{WithChatOptions(map[string]any{"top_k": -1})}, false},
		{"top k string", "qwen3", []Option{WithChatOptions(map[string]any{"top_k": "invalid"})}, false},
		{"top k fraction", "qwen3", []Option{WithChatOptions(map[string]any{"top_k": 1.5})}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter(NewClient("http://unused.invalid", tc.model, "", tc.opts...))
			err := a.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate=%v valid=%v", err, tc.valid)
			}
			if !tc.valid {
				_, err = a.ChatStream(context.Background(), nil, nil)
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("plain: %v", err)
				}
				_, err = a.ChatStreamWithSchema(context.Background(), nil, nil, &types.ParameterSchema{Type: "object"})
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("schema: %v", err)
				}
			}
		})
	}
}
