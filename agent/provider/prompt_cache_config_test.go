package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

func TestBuildPromptCacheMapping(t *testing.T) {
	ctx := context.Background()
	if _, err := Build(ctx, Config{Provider: OpenAI, Model: "gpt-4.1", APIKey: "k",
		PromptCache: &PromptCache{Mode: catalog.PromptCacheAutomatic, Retention: "24h"}}); err != nil {
		t.Fatalf("openai automatic cache: %v", err)
	}
	if _, err := Build(ctx, Config{Provider: Anthropic, Model: "claude-3-5-haiku", APIKey: "k",
		PromptCache: &PromptCache{Mode: catalog.PromptCacheMarkers, TTL: "5m", System: true}}); err != nil {
		t.Fatalf("anthropic markers: %v", err)
	}
	_, err := Build(ctx, Config{Provider: Google, Model: "gemini-2.5-flash", APIKey: "k",
		PromptCache: &PromptCache{Mode: catalog.PromptCacheMarkers, TTL: "5m", System: true}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("google markers must be rejected, got %v", err)
	}
	if _, err := Build(ctx, Config{Provider: Ollama, Model: "qwen3", PromptCache: &PromptCache{Mode: catalog.PromptCacheOff}}); err != nil {
		t.Fatalf("off is always accepted: %v", err)
	}
}

func TestAdaptersReportEffectiveOptions(t *testing.T) {
	temp, seed, budget := 0.5, int64(4), int64(512) // 0.5 survives Gemini's float32 round trip
	for _, cfg := range []Config{
		{Provider: OpenAI, Model: "gpt-4.1", APIKey: "k", Options: types.RequestOptions{Temperature: &temp, Seed: &seed}},
		{Provider: Anthropic, Model: "claude-3-5-haiku", APIKey: "k", Options: types.RequestOptions{Temperature: &temp}},
		{Provider: Google, Model: "gemini-2.5-flash", APIKey: "k", Options: types.RequestOptions{Temperature: &temp, ReasoningBudget: &budget}},
		{Provider: Ollama, Model: "qwen3", Options: types.RequestOptions{Temperature: &temp, Seed: &seed}},
	} {
		p, err := Build(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		o, ok := types.ProviderEffectiveOptions(p)
		if !ok || o.Temperature == nil || *o.Temperature != temp {
			t.Fatalf("%s: %+v", cfg.Provider, o)
		}
		if cfg.Provider == Anthropic && (o.MaxOutputTokens == nil || *o.MaxOutputTokens != 4096) {
			t.Fatalf("anthropic must report its max_tokens default: %+v", o)
		}
		if cfg.Provider == Google && (o.ReasoningBudget == nil || *o.ReasoningBudget != budget) {
			t.Fatalf("google thinking budget: %+v", o)
		}
	}
}
