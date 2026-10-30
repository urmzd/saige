package catalog

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestDefaultCatalogGeminiCosts checks the Gemini rate cards against
// hand-computed costs in micro-USD, on the endpoint a deployment calls and
// the model ID it sends, so a Vertex AI run on a preview name is costed
// instead of being counted as unpriced.
func TestDefaultCatalogGeminiCosts(t *testing.T) {
	chat := types.TokenUsage{InputTokens: 100_000, CachedInputTokens: 20_000, OutputTokens: 10_000}
	for _, tc := range []struct {
		name     string
		endpoint string
		model    string
		tier     types.ServiceTier
		usage    types.TokenUsage
		want     types.Cost
	}{
		// 0.1M x $0.50 + 0.02M x $0.05 + 0.01M x $3.00 = $0.081
		{"3 flash preview, Gemini API", "google-gemini", "gemini-3-flash-preview", "", chat, 81_000},
		{"3 flash preview, Vertex AI", "google-vertex", "gemini-3-flash-preview", "", chat, 81_000},
		// 0.1M x $0.25 + 0.02M x $0.05 (cache reads not discounted) + 0.01M x $1.50 = $0.041
		{"3 flash preview, batch", "google-gemini", "gemini-3-flash-preview", types.ServiceBatch, chat, 41_000},
		// 12,345 x $0.15 / 1M = $0.00185175
		{"embedding 001, Gemini API", "google-gemini", "gemini-embedding-001", "", types.TokenUsage{InputTokens: 12_345}, 1_852},
		{"embedding 001, Vertex AI", "google-vertex", "gemini-embedding-001", "", types.TokenUsage{InputTokens: 12_345}, 1_852},
		// 1M x $0.075 on the Gemini API batch, 1M x $0.12 on Vertex AI batch
		{"embedding 001, Gemini API batch", "google-gemini", "gemini-embedding-001", types.ServiceBatch, types.TokenUsage{InputTokens: 1_000_000}, 75_000},
		{"embedding 001, Vertex AI batch", "google-vertex", "gemini-embedding-001", types.ServiceBatch, types.TokenUsage{InputTokens: 1_000_000}, 120_000},
		// 0.2M x $0.10 + 0.05M x $0.01 + 0.04M x $0.40 = $0.0365
		{"2.5 flash lite", "google-vertex", "gemini-2.5-flash-lite",
			"", types.TokenUsage{InputTokens: 200_000, CachedInputTokens: 50_000, OutputTokens: 40_000}, 36_500},
		// 0.1M x $1.50 + 0.01M x $0.15 + 0.005M x $9.00 = $0.1965
		{"3.5 flash, Vertex AI", "google-vertex", "gemini-3.5-flash",
			"", types.TokenUsage{InputTokens: 100_000, CachedInputTokens: 10_000, OutputTokens: 5_000}, 196_500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, ok := LookupOffering(tc.endpoint, "google", tc.model)
			if !ok {
				t.Fatalf("no offering for %s on %s", tc.model, tc.endpoint)
			}
			p, ok := o.TierPricing(tc.tier)
			if !ok {
				t.Fatalf("tier %q not declared", tc.tier)
			}
			if p.Unpriced(tc.usage) {
				t.Fatalf("usage is unpriced at %+v", p)
			}
			if p.AsOf != "2026-10-30" || p.Source == "" {
				t.Fatalf("rate card provenance = %q from %q", p.AsOf, p.Source)
			}
			if got := p.Cost(tc.usage); got != tc.want {
				t.Fatalf("cost = %d micro-USD, want %d", got, tc.want)
			}
		})
	}
}

// TestGeminiPreviewResolvesToPricedRow checks that the preview model name a
// deployment sends matches the priced gemini-3-flash row by prefix, and that
// the capability path budgets read carries the same rates.
func TestGeminiPreviewResolvesToPricedRow(t *testing.T) {
	caps := MustLookup("google", "gemini-3-flash-preview")
	if caps.Family != "gemini-3-flash" {
		t.Fatalf("family = %q, want gemini-3-flash", caps.Family)
	}
	if p := caps.Pricing; p.InputPerMTok != 0.5 || p.OutputPerMTok != 3 || p.CachedInputPerMTok != 0.05 {
		t.Fatalf("pricing = %+v", p)
	}
	if p := MustLookup("google", "gemini-embedding-001").Pricing; p.InputPerMTok != 0.15 {
		t.Fatalf("embedding pricing = %+v", p)
	}
}

// TestGeminiUnlistedStaysUnpriced keeps models the vendor price lists do
// not name unpriced rather than guessed.
func TestGeminiUnlistedStaysUnpriced(t *testing.T) {
	for _, tc := range []struct{ endpoint, model string }{
		{"google-gemini", "gemini-3.5-flash"},
		{"google-gemini", "gemini-3-pro"},
		{"google-gemini", "gemini-3.1-flash"},
		{"google-gemini", "gemini-embedding-2"},
	} {
		o, ok := LookupOffering(tc.endpoint, "google", tc.model)
		if ok && !o.Pricing.IsZero() {
			t.Errorf("%s on %s is priced: %+v", tc.model, tc.endpoint, o.Pricing)
		}
	}
}
