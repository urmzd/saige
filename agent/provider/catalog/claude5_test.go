package catalog

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestClaudeFiveFamilies(t *testing.T) {
	for _, tt := range []struct {
		model    string
		required bool
		tier     Tier
	}{
		{"claude-haiku-5", false, TierEconomy},
		{"claude-haiku-5-5", false, TierEconomy},
		{"claude-sonnet-5", false, TierStandard},
		{"claude-sonnet-5-5", true, TierStandard},
		{"claude-opus-5", false, TierFrontier},
		{"claude-opus-5-5", true, TierFrontier},
		{"claude-fable-5", true, TierFrontier},
		{"claude-fable-5-1", true, TierFrontier},
	} {
		c, known := Lookup("anthropic", tt.model)
		if !known {
			t.Errorf("%s: not an exact row", tt.model)
			continue
		}
		if c.ReasoningRequired != tt.required || !c.ReasoningDefaultEnabled {
			t.Errorf("%s: required=%v default=%v", tt.model, c.ReasoningRequired, c.ReasoningDefaultEnabled)
		}
		if c.ContextWindow != 1_000_000 || c.MaxOutputTokens != 128_000 {
			t.Errorf("%s: limits %d/%d", tt.model, c.ContextWindow, c.MaxOutputTokens)
		}
		for _, k := range []types.ServerToolKind{types.ServerToolWebSearch, types.ServerToolCodeExecution, types.ServerToolRemoteMCP} {
			if !c.SupportsServerTool(k) {
				t.Errorf("%s: missing server tool %s", tt.model, k)
			}
		}
		if c.Supports(types.CapTemperature) || c.Supports(types.CapReasoningBudget) || c.Supports(types.CapAssistantPrefill) {
			t.Errorf("%s: declares sampling, budgets or prefill", tt.model)
		}
		if e, _ := Describe("anthropic", tt.model); e.Tier != tt.tier {
			t.Errorf("%s: tier %s", tt.model, e.Tier)
		}
	}
}
