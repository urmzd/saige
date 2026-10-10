package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Scenario (d): a model that takes tools on Chat Completions only without
// reasoning is served by the Responses API when a reasoning dial is on, so
// the effort survives tool calls. Reasoning off keeps Chat Completions.
func TestBuildServesReasoningDialOnResponses(t *testing.T) {
	keys := env(map[string]string{"OPENAI_API_KEY": "o"})
	high := types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}
	p, err := Build(context.Background(), Config{Model: "gpt-6-luna", Dials: high, Getenv: keys})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapper.As[*openai.ResponsesAdapter](p); !ok {
		t.Fatalf("got %T, want the Responses adapter", p)
	}
	off := types.Dials{Reasoning: &types.ReasoningDial{Mode: types.ReasoningOff}}
	if p, err = Build(context.Background(), Config{Model: "gpt-6-luna", Dials: off, Getenv: keys}); err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapper.As[*openai.Adapter](p); !ok {
		t.Fatalf("got %T, want Chat Completions", p)
	}
}

func TestBuildChecksDialsAndSelectsCache(t *testing.T) {
	keys := env(map[string]string{"ANTHROPIC_API_KEY": "a", "OPENAI_API_KEY": "o"})
	seed := int64(1)
	_, err := Build(context.Background(), Config{Model: "claude-haiku-5-5", Dials: types.Dials{Seed: &seed}, Getenv: keys})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("contractual dial: %v", err)
	}
	loose := types.DialPolicy{Per: map[types.DialName]types.Handling{types.DialReproducible: types.HandlingDrop}}
	if _, err := Build(context.Background(), Config{Model: "claude-haiku-5-5", Dials: types.Dials{Seed: &seed}, DialPolicy: &loose, Getenv: keys}); err != nil {
		t.Fatalf("a named loosening must build: %v", err)
	}
	on := true
	cfg, err := withDials(Config{Dials: types.Dials{Cache: &on}}, catalog.MustLookup("openai", "gpt-6-luna"))
	if err != nil || cfg.PromptCache == nil || cfg.PromptCache.Mode != "automatic" {
		t.Fatalf("cache dial: %v %+v", err, cfg.PromptCache)
	}
}
