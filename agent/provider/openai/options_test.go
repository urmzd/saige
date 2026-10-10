package openai

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestChatStreamWithOptions(t *testing.T) {
	temp, topK, seed := 0.4, 5.0, int64(9)
	for _, tc := range []struct {
		name       string
		configured []Option
		opts       types.RequestOptions
		wantChoice string
		wantFields map[string]any
		wantErr    bool
	}{
		{name: "required for one call", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}}, wantChoice: "required"},
		{name: "named", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "write"}},
			wantChoice: fmt.Sprint(map[string]any{"type": "function", "function": map[string]any{"name": "write"}})},
		{name: "per-call choice replaces the configured one",
			configured: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})},
			opts:       types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}, wantChoice: "none"},
		{name: "sampling overrides", opts: types.RequestOptions{Temperature: &temp, Seed: &seed},
			wantFields: map[string]any{"temperature": temp, "seed": float64(seed)}},
		{name: "top_k cannot be sent", opts: types.RequestOptions{TopK: &topK}, wantErr: true},
		{name: "unknown tool", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			a := NewAdapter("k", testModel, append(tc.configured, WithBaseURL(server.URL))...)
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: testTools, Options: new(tc.opts)})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) || len(*bodies) != 0 {
					t.Fatalf("err = %v with %d requests, want a local rejection", err, len(*bodies))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			body := (*bodies)[0]
			if tc.wantChoice != "" && fmt.Sprint(body["tool_choice"]) != tc.wantChoice {
				t.Fatalf("tool_choice = %v, want %v", body["tool_choice"], tc.wantChoice)
			}
			for k, v := range tc.wantFields {
				if body[k] != v {
					t.Fatalf("%s = %v, want %v", k, body[k], v)
				}
			}
			if tc.configured == nil && a.params.toolChoice != nil {
				t.Fatal("per-call options leaked into the adapter")
			}
		})
	}
}
