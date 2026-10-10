package anthropic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestChatStreamWithOptions(t *testing.T) {
	temp, seed, maxTokens := 0.3, int64(7), int64(256)
	for _, tc := range []struct {
		name       string
		configured []Option
		opts       types.RequestOptions
		wantChoice map[string]any
		wantFields map[string]any
		wantErr    bool
	}{
		{name: "forced choice for one call", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}},
			wantChoice: map[string]any{"type": "any"}},
		{name: "named choice", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}},
			wantChoice: map[string]any{"type": "tool", "name": "lookup"}},
		{name: "per-call choice replaces the configured one",
			configured: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})},
			opts:       types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}},
			wantChoice: map[string]any{"type": "none"}},
		{name: "sampling overrides", opts: types.RequestOptions{Temperature: &temp, MaxOutputTokens: &maxTokens},
			wantFields: map[string]any{"temperature": temp, "max_tokens": float64(maxTokens)}},
		{name: "undeclared option is rejected", opts: types.RequestOptions{Seed: &seed}, wantErr: true},
		{name: "unknown tool is rejected", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"}}, wantErr: true},
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
			drain(ch)
			body := (*bodies)[0]
			if tc.wantChoice != nil && fmt.Sprint(body["tool_choice"]) != fmt.Sprint(tc.wantChoice) {
				t.Fatalf("tool_choice = %v, want %v", body["tool_choice"], tc.wantChoice)
			}
			for k, v := range tc.wantFields {
				if body[k] != v {
					t.Fatalf("%s = %v, want %v", k, body[k], v)
				}
			}
			// The adapter itself is unchanged.
			if a.toolChoice != nil && tc.configured == nil {
				t.Fatal("per-call options leaked into the adapter")
			}
		})
	}
}

func TestPrefillCapability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		opts  []Option
		want  bool
	}{
		{"classic model", "claude-sonnet-4-20250514", nil, true},
		{"thinking configured", "claude-sonnet-4-20250514", []Option{WithThinking(2048), WithMaxTokens(8192)}, false},
		{"adaptive family", "claude-opus-5", nil, false},
		{"unknown model", "claude-unknown", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NewAdapter("k", tc.model, tc.opts...).Capabilities().Supports(types.CapAssistantPrefill)
			if got != tc.want {
				t.Fatalf("prefill = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrimPrefill(t *testing.T) {
	for _, tc := range []struct {
		name     string
		msgs     []types.Message
		wantLen  int
		wantText string
	}{
		{"trailing whitespace removed", []types.Message{types.UserMsg(types.Text("q")), types.AssistantMsg(types.Text("partial answer \n"))}, 2, "partial answer"},
		{"whitespace-only turn dropped", []types.Message{types.UserMsg(types.Text("q")), types.AssistantMsg(types.Text("  "))}, 1, ""},
		{"user last untouched", []types.Message{types.UserMsg(types.Text("q "))}, 1, "q "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out := toAnthropicParams(tc.msgs)
			if len(out) != tc.wantLen {
				t.Fatalf("messages = %d, want %d", len(out), tc.wantLen)
			}
			last := out[len(out)-1].Content
			if tc.wantText != "" && (last[len(last)-1].OfText == nil || last[len(last)-1].OfText.Text != tc.wantText) {
				t.Fatalf("last text = %+v, want %q", last[len(last)-1], tc.wantText)
			}
		})
	}
}
