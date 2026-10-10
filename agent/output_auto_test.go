package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// A model inferred from a family prefix still reports what its adapter
// accepts. claude-sonnet-5-5-2099 matches the claude-sonnet-5-5 row, which
// rejects a forced tool choice, so the Anthropic adapter cannot force its
// schema tool: auto mode must use the final_answer tool, not the native path
// that fails at call time. An adaptive model that accepts forcing keeps the
// native path.
func TestOutputAutoConsultsAdapterForInferredModel(t *testing.T) {
	p := anthropic.NewAdapter("key", "claude-sonnet-4-5-2099", anthropic.WithThinking(1024))
	if mc, _ := types.ProviderCapabilities(p); mc.Known {
		t.Fatal("test needs a prefix-inferred model")
	}
	a := must.Get(New(Config{Provider: p}))
	got, err := a.resolveOutputMode(OutputAuto, cityPopulationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if got != OutputTool {
		t.Fatalf("mode = %q, want %q", got, OutputTool)
	}
	plain := must.Get(New(Config{Provider: anthropic.NewAdapter("key", "claude-3-5-haiku-2099")}))
	if got, _ := plain.resolveOutputMode(OutputAuto, cityPopulationSchema); got != OutputNative {
		t.Fatalf("a non-thinking model keeps the native path, got %q", got)
	}
	adaptive := must.Get(New(Config{Provider: anthropic.NewAdapter("key", "claude-haiku-5-5-2099")}))
	if got, _ := adaptive.resolveOutputMode(OutputAuto, cityPopulationSchema); got != OutputNative {
		t.Fatalf("an adaptive model that accepts forcing keeps the native path, got %q", got)
	}
}

// A schema set with WithResponseSchema in auto mode uses native output where
// the adapter can apply it, including models that reject a forced tool
// choice, which take output_config.format, and the final_answer tool, which
// never forces a call, where the adapter cannot apply it.
func TestResponseSchemaAutoAvoidsForcedTool(t *testing.T) {
	for _, tc := range []struct {
		model string
		opts  []anthropic.Option
		want  OutputMode
	}{
		{"claude-haiku-5-5", nil, OutputNative},
		{"claude-sonnet-5-5", nil, OutputNative},
		{"claude-opus-5-5", nil, OutputNative},
		{"claude-fable-5-1", nil, OutputNative},
		{"claude-sonnet-4-5", []anthropic.Option{anthropic.WithThinking(1024)}, OutputTool},
	} {
		t.Run(tc.model, func(t *testing.T) {
			a := must.Get(New(Config{Provider: anthropic.NewAdapter("key", tc.model, tc.opts...)}, WithResponseSchema(cityPopulationSchema)))
			out := a.output(context.Background())
			if out.mode != tc.want {
				t.Fatalf("mode = %q, want %q", out.mode, tc.want)
			}
			if err := a.checkOutput(a.cfg.Provider, out); err != nil {
				t.Fatalf("checkOutput: %v", err)
			}
		})
	}
}
