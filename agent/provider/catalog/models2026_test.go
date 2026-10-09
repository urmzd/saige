package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestCurrentModelRows checks the declarations the current vendor rows carry:
// forced tool choice, the Chat Completions tool rule, effort ranges and
// pricing.
func TestCurrentModelRows(t *testing.T) {
	for _, tt := range []struct {
		provider, model string
		rejectsForced   bool
		chatTools       types.ChatCompletionsTools
		efforts         string
		input, output   float64
	}{
		{"anthropic", "claude-haiku-5-5", false, "", "low medium high xhigh max", 0.1, 0.5},
		{"anthropic", "claude-sonnet-5-5", true, "", "low medium high xhigh max", 2, 10},
		{"anthropic", "claude-opus-5-5", true, "", "low medium high xhigh max", 4, 20},
		{"anthropic", "claude-fable-5-1", true, "", "low medium high xhigh max", 10, 50},
		{"anthropic", "claude-mythos-5-1", true, "", "low medium high xhigh max", 10, 50},
		{"openai", "gpt-6-luna", false, types.ChatToolsNoReasoning, "none low medium high xhigh max", 0.1, 0.5},
		{"openai", "gpt-6-sol", false, types.ChatToolsNoReasoning, "none low medium high xhigh max", 2, 10},
		{"openai", "gpt-6.1-sol", false, types.ChatToolsResponsesOnly, "low medium high xhigh max", 2, 10},
		{"openai", "gpt-6-astra", false, types.ChatToolsResponsesOnly, "low medium high xhigh max", 10, 50},
		{"openai", "gpt-5.6-luna", false, "", "none low medium high xhigh max", 0.2, 1.2},
		{"google", "gemini-3.1-flash-lite", false, "", "minimal low medium high", 0.25, 1.5},
		{"google", "gemini-3.8-flash", false, "", "low medium high", 0.75, 3.75},
	} {
		c, known := Lookup(tt.provider, tt.model)
		if !known {
			t.Errorf("%s: not an exact row", tt.model)
			continue
		}
		if c.RejectsForcedToolChoice != tt.rejectsForced {
			t.Errorf("%s: rejects forced tool choice = %v", tt.model, c.RejectsForcedToolChoice)
		}
		if c.ChatCompletionsTools != tt.chatTools {
			t.Errorf("%s: chat tools rule %q", tt.model, c.ChatCompletionsTools)
		}
		if got := strings.Join(c.ReasoningEfforts, " "); got != tt.efforts {
			t.Errorf("%s: efforts %q", tt.model, got)
		}
		if c.Pricing.InputPerMTok != tt.input || c.Pricing.OutputPerMTok != tt.output || c.Pricing.AsOf != "2026-10-09" {
			t.Errorf("%s: pricing %+v", tt.model, c.Pricing)
		}
		for _, n := range c.Notes {
			if strings.HasPrefix(n, "unpriced") {
				t.Errorf("%s: still noted as unpriced", tt.model)
			}
		}
	}
}

// TestForcedToolChoiceRejectedLocally checks that a forced choice on a model
// that rejects it fails validation, while auto and none pass.
func TestForcedToolChoiceRejectedLocally(t *testing.T) {
	c := MustLookup("anthropic", "claude-sonnet-5-5")
	for _, mode := range []types.ToolChoiceMode{types.ToolChoiceRequired, types.ToolChoiceNamed} {
		choice := &types.ToolChoice{Mode: mode}
		if mode == types.ToolChoiceNamed {
			choice.Name = "x"
		}
		if err := c.ValidateToolChoice(choice, nil); !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Errorf("%s: err = %v", mode, err)
		}
	}
	for _, mode := range []types.ToolChoiceMode{types.ToolChoiceAuto, types.ToolChoiceNone} {
		if err := c.ValidateToolChoice(&types.ToolChoice{Mode: mode}, nil); err != nil {
			t.Errorf("%s: err = %v", mode, err)
		}
	}
	if err := MustLookup("anthropic", "claude-haiku-5-5").ValidateToolChoice(&types.ToolChoice{Mode: types.ToolChoiceRequired}, nil); err != nil {
		t.Errorf("haiku required: %v", err)
	}
}

// TestPresetSamplingRules checks that a preset setting temperature on a gpt-6
// model fails at load unless reasoning effort is none, and always fails on a
// model without effort none.
func TestPresetSamplingRules(t *testing.T) {
	for _, tt := range []struct {
		name, entry string
		ok          bool
	}{
		{"luna at the default effort", `{"provider":"openai","model":"gpt-6-luna","options":{"temperature":0.2}}`, false},
		{"luna with effort none", `{"provider":"openai","model":"gpt-6-luna","options":{"temperature":0.2,"reasoning":{"effort":"none"}}}`, true},
		{"sol 6.1", `{"provider":"openai","model":"gpt-6.1-sol","options":{"temperature":0.2}}`, false},
		{"sol 6.1 effort none", `{"provider":"openai","model":"gpt-6.1-sol","options":{"reasoning":{"effort":"none"}}}`, false},
		{"haiku 5.5", `{"provider":"anthropic","model":"claude-haiku-5-5","options":{"temperature":0.2}}`, false},
		{"vertex on a non-google entry", `{"provider":"openai","model":"gpt-6-luna","vertex":{}}`, false},
		{"vertex on google", `{"provider":"google","model":"gemini-3.1-flash-lite","vertex":{"project":"p"}}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			overlay, err := Load(strings.NewReader(`{"version":1,"presets":{"p":{"chain":[` + tt.entry + `]}}}`))
			if err != nil {
				t.Fatal(err)
			}
			merged, err := Merge(Default(), overlay)
			if err == nil {
				_, err = merged.Resolve("p")
			}
			if (err == nil) != tt.ok {
				t.Fatalf("load = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// TestDefaultPresets checks the cheap defaults and the quality tier.
func TestDefaultPresets(t *testing.T) {
	c := Default()
	want := map[string]string{
		"anthropic": "claude-haiku-5-5", "openai": "gpt-6-luna", "google": "gemini-3.1-flash-lite",
		"anthropic-quality": "claude-sonnet-5-5", "openai-quality": "gpt-6.1-sol", "google-quality": "gemini-3.8-flash",
		"vertex": "gemini-3.1-flash-lite",
	}
	for name, model := range want {
		rp, err := c.Resolve(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rp.Chain[0].Model != model {
			t.Errorf("%s: model %s, want %s", name, rp.Chain[0].Model, model)
		}
	}
	rp, err := c.Resolve(c.DefaultPreset)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rp.Chain {
		if m, ok := want[e.Provider]; ok && e.Model != m {
			t.Errorf("default chain %s: model %s, want %s", e.Provider, e.Model, m)
		}
	}
	if v, _ := c.Resolve("vertex"); v.Chain[0].Vertex == nil {
		t.Error("vertex preset has no vertex block")
	}
}
