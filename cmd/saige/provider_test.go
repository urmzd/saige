package main

import "testing"

// newTestFlags builds a commonFlags with every field set to the given values.
func newTestFlags(provider, model, embedProvider, embedModel string) *commonFlags {
	return &commonFlags{
		provider:      &provider,
		model:         &model,
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
