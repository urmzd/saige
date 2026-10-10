package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// A model that keeps prompt caches for 24 hours only rejects in_memory
// retention when the provider is built, before any request.
func TestBuildRejectsUndeclaredRetention(t *testing.T) {
	getenv := func(string) string { return "test-key" }
	for _, tc := range []struct {
		model, retention string
		ok               bool
	}{
		{"gpt-6-luna", "in_memory", false},
		{"gpt-6-luna", "in-memory", false},
		{"gpt-6-luna", "24h", true},
		{"gpt-6.1-sol", "in_memory", false},
		{"gpt-4.1", "in_memory", true},
	} {
		p, err := Build(context.Background(), Config{Provider: OpenAI, Model: types.ModelID(tc.model), Getenv: getenv,
			PromptCache: &PromptCache{Mode: "automatic", Retention: tc.retention}})
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s %s: %v", tc.model, tc.retention, err)
		case !tc.ok && !errors.Is(err, types.ErrInvalidModelConfig):
			t.Errorf("%s %s: built (%v)", tc.model, tc.retention, err)
		}
		if p != nil {
			_ = types.CloseProvider(context.Background(), p)
		}
	}
}
