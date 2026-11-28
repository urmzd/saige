package catalog

import "testing"

// The Claude 4 family rows carry the output limit the API reports for the
// models they still resolve (GET /v1/models, 2026-11-27). A caller that sets a
// request's output cap from the catalog reads 0 as "unknown" and falls back
// to the adapter's small default, which cuts a long tool call off.
//
// The rows are family prefixes. claude-opus-4 and claude-sonnet-4 also match
// the identifiers of retired models (claude-opus-4-1, claude-sonnet-4-0),
// which the API no longer serves and whose limits it no longer reports.
// claude-3-7-sonnet is retired as a whole and stays unknown.
func TestClaudeFourFamilyOutputLimits(t *testing.T) {
	for _, tt := range []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 64_000},
		{"claude-haiku-4-5-20251001", 64_000},
		{"claude-sonnet-4-5", 64_000},
		{"claude-sonnet-4-5-20250929", 64_000},
		{"claude-opus-4-5", 64_000},
		{"claude-opus-4-5-20251101", 64_000},
		// Rows of their own, unchanged.
		{"claude-opus-4-6", 128_000},
		{"claude-opus-4-7", 128_000},
		{"claude-opus-4-8", 128_000},
		{"claude-sonnet-4-6", 128_000},
		// Retired: no figure could be verified.
		{"claude-3-7-sonnet-20250219", 0},
	} {
		c, _ := Lookup("anthropic", tt.model)
		if c.MaxOutputTokens != tt.want {
			t.Errorf("%s: max output tokens = %d, want %d", tt.model, c.MaxOutputTokens, tt.want)
		}
		if c.ContextWindow == 0 {
			t.Errorf("%s: no context window", tt.model)
		}
	}
}
