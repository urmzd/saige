package openai

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// TestChatCompletionsToolRule checks the catalog's Chat Completions tool rule
// on the wire: a model that accepts tools there only with reasoning effort
// none gets that effort when tools are offered and none is set, keeps its
// default without tools, and rejects another effort with tools before any
// request. A model whose tools need the Responses API rejects tools here.
func TestChatCompletionsToolRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		opts       []Option
		tools      []types.ToolDef
		wantEffort any // nil means reasoning_effort is absent
		wantErr    bool
	}{
		{name: "tools without effort send none", model: "gpt-6-luna", tools: testTools, wantEffort: "none"},
		{name: "no tools keep the default", model: "gpt-6-luna"},
		{name: "explicit none with tools", model: "gpt-6-luna", opts: []Option{WithReasoningEffort("none")}, tools: testTools, wantEffort: "none"},
		{name: "explicit effort without tools", model: "gpt-6-luna", opts: []Option{WithReasoningEffort("high")}, wantEffort: "high"},
		{name: "effort with tools is rejected", model: "gpt-6-luna", opts: []Option{WithReasoningEffort("low")}, tools: testTools, wantErr: true},
		{name: "responses-only model rejects tools", model: "gpt-6.1-sol", tools: testTools, wantErr: true},
		{name: "responses-only model without tools", model: "gpt-6.1-sol"},
		{name: "unrestricted model", model: "gpt-5.6-luna", tools: testTools},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			a := must.Get(New(Config{APIKey: "k", Model: types.ModelID(tc.model)}, append([]Option{WithBaseURL(server.URL)}, tc.opts...)...))
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: tc.tools})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want an invalid configuration", err)
				}
				if len(*bodies) != 0 {
					t.Fatal("a rejected request reached the server")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			if got := (*bodies)[0]["reasoning_effort"]; got != tc.wantEffort {
				t.Fatalf("reasoning_effort = %v, want %v", got, tc.wantEffort)
			}
		})
	}
}

// TestSamplingNeedsNoReasoning checks the gpt-6 sampling rule: temperature is
// accepted only with reasoning effort none, and never on a model without
// that effort.
func TestSamplingNeedsNoReasoning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		opts  []Option
		ok    bool
	}{
		{"temperature at the default effort", "gpt-6-luna", []Option{WithTemperature(0.2)}, false},
		{"temperature with effort none", "gpt-6-luna", []Option{WithTemperature(0.2), WithReasoningEffort("none")}, true},
		{"temperature on a model without effort none", "gpt-6.1-sol", []Option{WithTemperature(0.2)}, false},
		{"effort none on a model without it", "gpt-6.1-sol", []Option{WithReasoningEffort("none")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := must.Get(New(Config{APIKey: "k", Model: types.ModelID(tc.model)}, tc.opts...)).Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
