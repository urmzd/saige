package catalog

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func dialCreativity(c types.Creativity) *types.Creativity { return &c }
func dialDepth(d types.Depth) *types.ReasoningDial        { return &types.ReasoningDial{Depth: d} }
func dialMode(m types.ReasoningMode) *types.ReasoningDial {
	return &types.ReasoningDial{Mode: m}
}

// TestDialsOverDefaultCatalog pins (model, dial) to (raw, action) for the
// default catalog, including the rule that reasoning outranks creativity.
func TestDialsOverDefaultCatalog(t *testing.T) {
	focused := types.Dials{Creativity: dialCreativity(types.CreativityFocused)}
	for _, tc := range []struct {
		provider, model string
		dials           types.Dials
		ctx             types.DialContext
		dial            types.DialName
		action          types.DialAction
		sent            string
	}{
		// Scenario (a): creativity on a failover chain.
		{"openai", "gpt-6-luna", focused, types.DialContext{}, types.DialCreativity, types.DialDropped, ""},
		{"anthropic", "claude-haiku-5-5", focused, types.DialContext{}, types.DialCreativity, types.DialDropped, ""},
		{"openai", "gpt-4.1", focused, types.DialContext{}, types.DialCreativity, types.DialApplied, "temperature=0.3 top_p=0.9"},
		{"google", "gemini-3.8-flash", focused, types.DialContext{}, types.DialCreativity, types.DialApplied, "temperature=0.3 top_p=0.9"},
		{"openai", "gpt-6-luna", types.Dials{Creativity: dialCreativity(types.CreativityFocused), Reasoning: dialMode(types.ReasoningOff)},
			types.DialContext{}, types.DialCreativity, types.DialApplied, "temperature=0.3 top_p=0.9"},
		// Scenario (c): depth high across vendors.
		{"anthropic", "claude-haiku-5-5", types.Dials{Reasoning: dialDepth(types.DepthHigh)}, types.DialContext{}, types.DialReasoning, types.DialApplied, "effort=high"},
		{"openai", "gpt-6.1-sol", types.Dials{Reasoning: dialDepth(types.DepthHigh)}, types.DialContext{}, types.DialReasoning, types.DialApplied, "effort=high"},
		{"google", "gemini-3.8-flash", types.Dials{Reasoning: dialDepth(types.DepthHigh)}, types.DialContext{}, types.DialReasoning, types.DialApplied, "effort=high"},
		{"ollama", "qwen3.5:4b", types.Dials{Reasoning: dialDepth(types.DepthHigh)}, types.DialContext{}, types.DialReasoning, types.DialMapped, "think=true"},
		{"google", "gemini-3.8-flash", types.Dials{Reasoning: dialDepth(types.DepthMax)}, types.DialContext{}, types.DialReasoning, types.DialMapped, "effort=high"},
		{"openai", "gpt-6.1-sol", types.Dials{Reasoning: dialMode(types.ReasoningOff)}, types.DialContext{}, types.DialReasoning, types.DialMapped, "effort=low"},
		{"google", "gemini-3.8-flash", types.Dials{Reasoning: dialMode(types.ReasoningOff)}, types.DialContext{}, types.DialReasoning, types.DialMapped, "effort=low"},
		{"google", "gemini-3.1-flash-lite", types.Dials{Reasoning: dialMode(types.ReasoningOff)}, types.DialContext{}, types.DialReasoning, types.DialMapped, "effort=minimal"},
		// Scenario (d): tools on the Chat Completions surface.
		{"openai", "gpt-6-luna", types.Dials{Reasoning: dialDepth(types.DepthHigh)}, types.DialContext{Tools: true, Surface: types.SurfaceChat},
			types.DialReasoning, types.DialMapped, "effort=none"},
		{"openai", "gpt-6-luna", types.Dials{Reasoning: dialDepth(types.DepthHigh)}, types.DialContext{Tools: true, Surface: types.SurfaceResponses},
			types.DialReasoning, types.DialApplied, "effort=high"},
		// Scenario (b): creativity is advisory, a seed contractual.
		{"anthropic", "claude-3-5-haiku", types.Dials{Creativity: dialCreativity(types.CreativityDeterministic)}, types.DialContext{},
			types.DialCreativity, types.DialApplied, "temperature=0"},
		{"openai", "gpt-4.1", types.Dials{Seed: new(int64)}, types.DialContext{}, types.DialReproducible, types.DialApplied, "seed=0"},
	} {
		t.Run(tc.provider+"/"+tc.model+"/"+string(tc.dial), func(t *testing.T) {
			mc := MustLookup(tc.provider, tc.model)
			eff, rep, err := types.ResolveDials(mc, types.RequestOptions{}, tc.ctx, types.DialPolicy{}, types.DialLayer{Scope: types.DialScopeAgent, Dials: tc.dials})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			d, ok := rep.Decision(tc.dial)
			if !ok || d.Action != tc.action || d.Sent != tc.sent {
				t.Fatalf("got %+v, want %s %q", d, tc.action, tc.sent)
			}
			if err := Expressible(tc.provider, eff); err != nil {
				t.Fatalf("not expressible: %v", err)
			}
		})
	}
}

// Scenario (b): a seed is contractual, so a model without one is rejected.
func TestDialSeedRejectedWithoutSeed(t *testing.T) {
	seed := int64(7)
	_, _, err := types.ResolveDials(MustLookup("anthropic", "claude-haiku-5-5"), types.RequestOptions{}, types.DialContext{}, types.DialPolicy{},
		types.DialLayer{Dials: types.Dials{Seed: &seed}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("seed on anthropic: %v", err)
	}
}

// allDialValues enumerates every value of every dial, one dial at a time.
func allDialValues() []types.Dials {
	var out []types.Dials
	for _, c := range []types.Creativity{types.CreativityDeterministic, types.CreativityFocused, types.CreativityBalanced, types.CreativityCreative} {
		out = append(out, types.Dials{Creativity: dialCreativity(c)})
	}
	for _, m := range []types.ReasoningMode{types.ReasoningOff, types.ReasoningAdaptive, types.ReasoningOn} {
		out = append(out, types.Dials{Reasoning: dialMode(m)})
	}
	for _, d := range []types.Depth{types.DepthMinimal, types.DepthLow, types.DepthMedium, types.DepthHigh, types.DepthMax} {
		out = append(out, types.Dials{Reasoning: dialDepth(d)})
	}
	big, small, on, off, seed := int64(1<<30), int64(256), true, false, int64(3)
	out = append(out, types.Dials{MaxOutput: &big}, types.Dials{MaxOutput: &small}, types.Dials{Parallel: &on}, types.Dials{Parallel: &off},
		types.Dials{Seed: &seed}, types.Dials{Cache: &on}, types.Dials{Cache: &off},
		types.Dials{Tools: &types.ToolChoice{Mode: types.ToolChoiceNone}}, types.Dials{Tools: &types.ToolChoice{Mode: types.ToolChoiceRequired}})
	return out
}

// checkCompiled asserts the compiler's contract for one model and dials:
// the effective options pass validation and the adapter can send them, or
// the error matches ErrInvalidModelConfig.
func checkCompiled(t *testing.T, e Entry, d types.Dials, ctx types.DialContext) {
	t.Helper()
	mc := e.Caps.ForModel(e.Prefix)
	mc.Provider = e.Provider
	eff, rep, err := types.ResolveDials(mc, types.RequestOptions{}, ctx, types.DialPolicy{}, types.DialLayer{Scope: types.DialScopeEntry, Dials: d})
	if err != nil {
		if !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Fatalf("%s/%s %+v: error outside ErrInvalidModelConfig: %v", e.Provider, e.Prefix, d, err)
		}
		return
	}
	if err := mc.ValidateOptions(eff); err != nil {
		t.Fatalf("%s/%s %+v: effective options invalid: %v (%+v)", e.Provider, e.Prefix, d, err, rep.Decisions)
	}
	if err := Expressible(e.Provider, eff); err != nil {
		t.Fatalf("%s/%s %+v: effective options not expressible: %v (%+v)", e.Provider, e.Prefix, d, err, rep.Decisions)
	}
}

func TestDialsCompileEveryDefaultRow(t *testing.T) {
	cat := mustDefault()
	for _, e := range cat.view().entries {
		for _, d := range allDialValues() {
			for _, ctx := range []types.DialContext{{}, {Tools: true, Surface: types.SurfaceChat}} {
				checkCompiled(t, e, d, ctx)
			}
		}
	}
}

func FuzzResolveDials(f *testing.F) {
	f.Add(uint16(0), uint16(0), uint16(0), false)
	f.Add(uint16(3), uint16(7), uint16(12), true)
	f.Fuzz(func(t *testing.T, row, a, b uint16, tools bool) {
		entries := mustDefault().view().entries
		values := allDialValues()
		e := entries[int(row)%len(entries)]
		d := values[int(a)%len(values)].Merge(values[int(b)%len(values)])
		checkCompiled(t, e, d, types.DialContext{Tools: tools, Surface: types.SurfaceChat})
	})
}
