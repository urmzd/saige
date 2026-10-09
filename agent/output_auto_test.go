package agent

import (
	"testing"

	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/types"
)

// A model inferred from a family prefix still reports what its adapter
// accepts. claude-sonnet-5-9 matches the claude-sonnet-5 row, which thinks by
// default, so the Anthropic adapter cannot force its schema tool: auto mode
// must use the final_answer tool, not the native path that fails at call time.
func TestOutputAutoConsultsAdapterForInferredModel(t *testing.T) {
	p := anthropic.NewAdapter("key", "claude-sonnet-5-9")
	if mc, _ := types.ProviderCapabilities(p); mc.Known {
		t.Fatal("test needs a prefix-inferred model")
	}
	a := NewAgent(AgentConfig{Provider: p})
	got, err := a.resolveOutputMode(OutputAuto, cityPopulationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if got != OutputTool {
		t.Fatalf("mode = %q, want %q", got, OutputTool)
	}
	plain := NewAgent(AgentConfig{Provider: anthropic.NewAdapter("key", "claude-3-5-haiku-2099")})
	if got, _ := plain.resolveOutputMode(OutputAuto, cityPopulationSchema); got != OutputNative {
		t.Fatalf("a non-thinking model keeps the native path, got %q", got)
	}
}
