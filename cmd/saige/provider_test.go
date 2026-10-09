package main

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// newTestFlags builds a commonFlags with every field set to the given values.
func newTestFlags(provider, model, embedProvider, embedModel string) *commonFlags {
	return &commonFlags{
		provider:      &provider,
		model:         &model,
		preset:        new(string),
		catalogs:      new([]string),
		system:        new(string),
		ollamaHost:    new(string),
		baseURL:       new(string),
		embedProvider: &embedProvider,
		embedModel:    &embedModel,
		ragDB:         new(string),
		kgDB:          new(string),
		format:        new(string),
	}
}

func clearProviderEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
}

func TestResolvedEmbedProvider(t *testing.T) {
	clearProviderEnv(t)

	tests := []struct {
		name          string
		provider      string
		embedProvider string
		want          string
	}{
		{"unset follows LLM provider", providerAnthropic, "", providerAnthropic},
		{"override decouples from LLM provider", providerAnthropic, providerOllama, providerOllama},
		{"override with same provider", providerOpenAI, providerOpenAI, providerOpenAI},
		{"both unset falls back to auto-detect default", "", "", providerOllama},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newTestFlags(tt.provider, "", tt.embedProvider, "")
			if got := cf.resolvedEmbedProvider(); got != tt.want {
				t.Errorf("resolvedEmbedProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolvedEmbedModel(t *testing.T) {
	clearProviderEnv(t)

	tests := []struct {
		name          string
		provider      string
		embedProvider string
		embedModel    string
		want          string
	}{
		{"explicit model wins", providerOllama, providerOpenAI, "custom-embed", "custom-embed"},
		{"default follows embed provider", providerAnthropic, providerOpenAI, "", defaultEmbedModels[providerOpenAI]},
		{"default follows LLM provider when embed provider unset", providerGoogle, "", "", defaultEmbedModels[providerGoogle]},
		{"anthropic embed default is empty", providerAnthropic, "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newTestFlags(tt.provider, "", tt.embedProvider, tt.embedModel)
			if got := cf.resolvedEmbedModel(); got != tt.want {
				t.Errorf("resolvedEmbedModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveProviderWrapsHostedProviders(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("GEMINI_API_KEY", "")
	tests := []struct {
		name      string
		provider  string
		env       string
		wantRetry bool
		wantErr   bool
	}{
		{name: "openai", provider: providerOpenAI, env: "OPENAI_API_KEY", wantRetry: true},
		{name: "anthropic", provider: providerAnthropic, env: "ANTHROPIC_API_KEY", wantRetry: true},
		{name: "ollama stays bare", provider: providerOllama},
		{name: "missing key", provider: providerAnthropic, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv(tt.env, "test-key")
			}
			cf := newTestFlags(tt.provider, "", "", "")
			b, err := resolveBundle(context.Background(), cf, false)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want an error for a missing key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			members := b.Session().(interface{ Unwrap() []types.Provider }).Unwrap()
			if len(members) != 1 {
				t.Fatalf("members = %d, want the provider's one-entry preset", len(members))
			}
			p := members[0]
			if _, ok := p.(*retry.Provider); ok != tt.wantRetry {
				t.Fatalf("retry wrapped = %v, want %v (%T)", ok, tt.wantRetry, p)
			}
			if got := types.ProviderName(wrapper.Innermost(p)); got != tt.provider {
				t.Fatalf("provider name = %q, want %q", got, tt.provider)
			}
		})
	}
}
