package types_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func creativity(c types.Creativity) *types.Creativity { return &c }
func depth(d types.Depth) *types.ReasoningDial        { return &types.ReasoningDial{Depth: d} }
func mode(m types.ReasoningMode) *types.ReasoningDial {
	return &types.ReasoningDial{Mode: m}
}

// capsOf builds a declaration from a capability list.
func capsOf(provider string, list ...types.Capability) types.ModelCapabilities {
	m := map[types.Capability]bool{}
	for _, c := range list {
		m[c] = true
	}
	return types.ModelCapabilities{Provider: provider, Model: "m", Caps: m}
}

// effortModel takes sampling only with reasoning off, like a hybrid
// reasoning model that defaults to effort medium.
func effortModel() types.ModelCapabilities {
	mc := capsOf("openai", types.CapTemperature, types.CapTopP, types.CapReasoning, types.CapReasoningEffort,
		types.CapMaxOutputTokens, types.CapSeed, types.CapToolChoice, types.CapParallelToolControl)
	mc.ReasoningEfforts = []string{"none", "low", "medium", "high"}
	mc.DefaultReasoningEffort = "medium"
	mc.SamplingRequiresNoReasoning = []types.Capability{types.CapTemperature, types.CapTopP}
	mc.MaxOutputTokens = 1000
	return mc
}

func resolve(t *testing.T, mc types.ModelCapabilities, raw types.RequestOptions, pol types.DialPolicy, d types.Dials) (types.RequestOptions, types.DialReport) {
	t.Helper()
	eff, rep, err := types.ResolveDials(mc, raw, types.DialContext{}, pol, types.DialLayer{Scope: types.DialScopeAgent, Dials: d})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return eff, rep
}

func decision(t *testing.T, rep types.DialReport, name types.DialName) types.DialDecision {
	t.Helper()
	d, ok := rep.Decision(name)
	if !ok {
		t.Fatalf("no decision for %s in %+v", name, rep.Decisions)
	}
	return d
}

func TestDialsMergeFieldLevelReasoningWhole(t *testing.T) {
	base := types.Dials{Creativity: creativity(types.CreativityFocused), Reasoning: &types.ReasoningDial{Mode: types.ReasoningOn, Depth: types.DepthLow}}
	over := types.Dials{Reasoning: depth(types.DepthHigh), MaxOutput: i64(50)}
	got := base.Merge(over)
	if *got.Creativity != types.CreativityFocused || *got.MaxOutput != 50 {
		t.Fatalf("field merge: %+v", got)
	}
	if got.Reasoning.Mode != "" || got.Reasoning.Depth != types.DepthHigh {
		t.Fatalf("reasoning must merge whole: %+v", got.Reasoning)
	}
	got.Reasoning.Depth = types.DepthMax
	if over.Reasoning.Depth != types.DepthHigh {
		t.Fatal("merge aliases its input")
	}
	if names := got.Names(); !reflect.DeepEqual(names, []types.DialName{types.DialCreativity, types.DialReasoning, types.DialMaxOutput}) {
		t.Fatalf("names: %v", names)
	}
}

func TestRequestOptionsMergeCarriesDials(t *testing.T) {
	cfg := types.RequestOptions{DialLayers: []types.DialLayer{{Scope: types.DialScopeEntry, Dials: types.Dials{Seed: i64(1)}}}}
	req := types.RequestOptions{Dials: types.Dials{Seed: i64(2)},
		DialLayers: []types.DialLayer{{Scope: types.DialScopeAgent, Dials: types.Dials{Parallel: new(bool)}}},
		DialPolicy: &types.StrictDials}
	got := cfg.Merge(req)
	layers := got.Layers()
	if len(layers) != 3 || layers[0].Scope != types.DialScopeEntry || layers[1].Scope != types.DialScopeAgent || layers[2].Scope != types.DialScopeRequest {
		t.Fatalf("layers: %+v", layers)
	}
	if got.DialPolicy == nil || !got.DialPolicy.Strict || !got.HasDials() {
		t.Fatalf("policy or dials lost: %+v", got)
	}
	if raw := got.Raw(); raw.HasDials() || raw.DialPolicy != nil {
		t.Fatalf("raw keeps dials: %+v", raw)
	}
	got.DialLayers[0].Dials.Seed = i64(9)
	if *cfg.DialLayers[0].Dials.Seed != 1 {
		t.Fatal("merge aliases the base layers")
	}
}

func TestDialsValidateRejectsUnknownValues(t *testing.T) {
	for _, d := range []types.Dials{
		{Creativity: creativity("wild")},
		{Reasoning: &types.ReasoningDial{Mode: "sometimes"}},
		{Reasoning: &types.ReasoningDial{Depth: "deep"}},
		{Reasoning: &types.ReasoningDial{Mode: types.ReasoningOff, Depth: types.DepthHigh}},
		{Reasoning: &types.ReasoningDial{}},
		{MaxOutput: i64(0)},
		{Tools: &types.ToolChoice{Mode: types.ToolChoiceNamed}},
	} {
		_, _, err := types.ResolveDials(effortModel(), types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, types.DialLayer{Dials: d})
		if !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Errorf("%+v: err = %v", d, err)
		}
	}
}

// Reasoning outranks creativity: an active default effort drops sampling,
// and an explicit reasoning off lets it through.
func TestCreativityConflictRule(t *testing.T) {
	_, rep := resolve(t, effortModel(), types.RequestOptions{}, types.DialPolicy{}, types.Dials{Creativity: creativity(types.CreativityFocused)})
	if d := decision(t, rep, types.DialCreativity); d.Action != types.DialDropped || d.Reason != "conflicts with effort medium" {
		t.Fatalf("creativity under default effort: %+v", d)
	}
	eff, rep := resolve(t, effortModel(), types.RequestOptions{}, types.DialPolicy{},
		types.Dials{Creativity: creativity(types.CreativityFocused), Reasoning: mode(types.ReasoningOff)})
	if d := decision(t, rep, types.DialCreativity); d.Action != types.DialApplied || d.Sent != "temperature=0.3 top_p=0.9" {
		t.Fatalf("creativity with reasoning off: %+v", d)
	}
	if *eff.ReasoningEffort != "none" || *eff.Temperature != 0.3 {
		t.Fatalf("effective: %+v", eff)
	}
}

func TestRawOptionOverridesDialAndStaysStrict(t *testing.T) {
	raw := types.RequestOptions{ReasoningEffort: str("low")}
	eff, rep := resolve(t, effortModel(), raw, types.DialPolicy{}, types.Dials{Reasoning: depth(types.DepthHigh)})
	if d := decision(t, rep, types.DialReasoning); d.Action != types.DialRawOverride || d.Sent != "effort=low" {
		t.Fatalf("override: %+v", d)
	}
	if *eff.ReasoningEffort != "low" {
		t.Fatalf("raw lost: %+v", eff)
	}
	// A raw value the model rejects is an error, never dropped (D-12).
	_, _, err := types.ResolveDials(effortModel(), types.RequestOptions{Temperature: f64(0.2)}, types.DialContext{}, types.DialPolicy{},
		types.DialLayer{Dials: types.Dials{Creativity: creativity(types.CreativityFocused)}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("raw temperature with active reasoning: %v", err)
	}
}

func TestMaxOutputClampsAndContractualSeed(t *testing.T) {
	eff, rep := resolve(t, effortModel(), types.RequestOptions{}, types.DialPolicy{}, types.Dials{MaxOutput: i64(5000), Seed: i64(7)})
	if d := decision(t, rep, types.DialMaxOutput); d.Action != types.DialMapped || *eff.MaxOutputTokens != 1000 {
		t.Fatalf("clamp: %+v %+v", d, eff)
	}
	if d := decision(t, rep, types.DialReproducible); d.Action != types.DialApplied || *eff.Seed != 7 {
		t.Fatalf("seed: %+v", d)
	}
	noSeed := effortModel().Without(types.CapSeed)
	_, rep, err := types.ResolveDials(noSeed, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, types.DialLayer{Dials: types.Dials{Seed: i64(7)}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("seed on a model without one: %v", err)
	}
	if d := decision(t, rep, types.DialReproducible); d.Action != types.DialRejected {
		t.Fatalf("rejection not recorded: %+v", d)
	}
}

func TestReasoningDerivedMappings(t *testing.T) {
	budget := capsOf("anthropic", types.CapReasoning, types.CapReasoningBudget, types.CapMaxOutputTokens)
	budget.MinReasoningBudget = 1024
	toggle := capsOf("ollama", types.CapReasoning, types.CapReasoningToggle)
	none := capsOf("openai", types.CapTemperature)
	for _, tc := range []struct {
		name   string
		mc     types.ModelCapabilities
		raw    types.RequestOptions
		dial   *types.ReasoningDial
		action types.DialAction
		sent   string
	}{
		{"effort by name", effortModel(), types.RequestOptions{}, depth(types.DepthHigh), types.DialApplied, "effort=high"},
		{"effort nearest", effortModel(), types.RequestOptions{}, depth(types.DepthMax), types.DialMapped, "effort=high"},
		{"effort off", effortModel(), types.RequestOptions{}, mode(types.ReasoningOff), types.DialApplied, "effort=none"},
		{"adaptive keeps the default", effortModel(), types.RequestOptions{}, mode(types.ReasoningAdaptive), types.DialApplied, ""},
		{"budget", budget, types.RequestOptions{MaxOutputTokens: i64(64000)}, depth(types.DepthHigh), types.DialApplied, "budget=16384"},
		{"budget below the cap", budget, types.RequestOptions{MaxOutputTokens: i64(4096)}, depth(types.DepthHigh), types.DialMapped, "budget=2048"},
		{"budget off by default", budget, types.RequestOptions{}, mode(types.ReasoningOff), types.DialApplied, ""},
		{"toggle collapses depth", toggle, types.RequestOptions{}, depth(types.DepthHigh), types.DialMapped, "think=true"},
		{"toggle off", toggle, types.RequestOptions{}, mode(types.ReasoningOff), types.DialApplied, "think=false"},
		{"no reasoning", none, types.RequestOptions{}, depth(types.DepthHigh), types.DialDropped, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rep := resolve(t, tc.mc, tc.raw, types.DialPolicy{}, types.Dials{Reasoning: tc.dial})
			if d := decision(t, rep, types.DialReasoning); d.Action != tc.action || d.Sent != tc.sent {
				t.Fatalf("got %+v, want %s %q", d, tc.action, tc.sent)
			}
		})
	}
}

func TestReasoningWithToolsOnChatSurface(t *testing.T) {
	mc := effortModel()
	mc.ChatCompletionsTools = types.ChatToolsNoReasoning
	layer := types.DialLayer{Dials: types.Dials{Reasoning: depth(types.DepthHigh)}}
	eff, rep, err := types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{Tools: true, Surface: types.SurfaceChat}, types.DialPolicy{}, layer)
	if err != nil || *eff.ReasoningEffort != "none" {
		t.Fatalf("chat with tools: %v %+v", err, eff)
	}
	if d := decision(t, rep, types.DialReasoning); d.Action != types.DialMapped {
		t.Fatalf("decision: %+v", d)
	}
	eff, _, err = types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{Tools: true, Surface: types.SurfaceResponses}, types.DialPolicy{}, layer)
	if err != nil || *eff.ReasoningEffort != "high" {
		t.Fatalf("responses keeps the effort: %v %+v", err, eff)
	}
}

// An open signed tool loop defers a reasoning mode change, and a depth
// change unless the row declares per-request depth.
func TestReasoningHoldDefers(t *testing.T) {
	keep := depth(types.DepthLow)
	modeChange := types.DialLayer{Scope: types.DialScopeTurn, Dials: types.Dials{Reasoning: mode(types.ReasoningOff)}, Hold: keep}
	eff, rep, err := types.ResolveDials(effortModel(), types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, modeChange)
	if err != nil || *eff.ReasoningEffort != "low" {
		t.Fatalf("mode change applied inside the loop: %v %+v", err, eff)
	}
	if d := decision(t, rep, types.DialReasoning); d.Action != types.DialDeferred || d.Requested != "off" || d.Sent != "effort=low" {
		t.Fatalf("decision: %+v", d)
	}
	depthChange := types.DialLayer{Scope: types.DialScopeTurn, Dials: types.Dials{Reasoning: depth(types.DepthHigh)}, Hold: keep}
	eff, _, _ = types.ResolveDials(effortModel(), types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, depthChange)
	if *eff.ReasoningEffort != "low" {
		t.Fatalf("depth change applied on a next-turn row: %+v", eff)
	}
	perRequest := effortModel()
	perRequest.DialMap.Reasoning = &types.ReasoningMap{DepthChange: types.DepthChangePerRequest}
	eff, _, _ = types.ResolveDials(perRequest, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, depthChange)
	if *eff.ReasoningEffort != "high" {
		t.Fatalf("per-request row deferred a depth change: %+v", eff)
	}
}

func TestCacheResetExpected(t *testing.T) {
	mc := effortModel()
	mc.DialMap.Reasoning = &types.ReasoningMap{ChangeResetsCache: true}
	prev := types.RequestOptions{ReasoningEffort: str("low")}
	_, rep, err := types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{Previous: &prev}, types.DialPolicy{},
		types.DialLayer{Dials: types.Dials{Reasoning: depth(types.DepthHigh)}})
	if err != nil {
		t.Fatal(err)
	}
	if d := decision(t, rep, types.DialReasoning); !d.CacheResetExpected {
		t.Fatalf("cache reset not reported: %+v", d)
	}
}

func TestCacheDialOnlyAtBuildScopes(t *testing.T) {
	mc := capsOf("openai", types.CapPromptCaching, types.CapAutomaticPromptCache)
	on := true
	_, rep, err := types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{},
		types.DialLayer{Scope: types.DialScopeEntry, Dials: types.Dials{Cache: &on}})
	if err != nil {
		t.Fatal(err)
	}
	if d := decision(t, rep, types.DialCache); d.Action != types.DialApplied || d.Sent != "prompt_cache=automatic" {
		t.Fatalf("entry cache: %+v", d)
	}
	_, rep, _ = types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{},
		types.DialLayer{Scope: types.DialScopeTurn, Dials: types.Dials{Cache: &on}})
	if d := decision(t, rep, types.DialCache); d.Action != types.DialDropped {
		t.Fatalf("turn cache: %+v", d)
	}
}

func TestDialScopesAndDeterminism(t *testing.T) {
	layers := []types.DialLayer{
		{Scope: types.DialScopePreset, Dials: types.Dials{Reasoning: depth(types.DepthLow), MaxOutput: i64(100)}},
		{Scope: types.DialScopeTurn, Dials: types.Dials{Reasoning: depth(types.DepthHigh)}},
	}
	_, a, err := types.ResolveDials(effortModel(), types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, layers...)
	if err != nil {
		t.Fatal(err)
	}
	_, b, _ := types.ResolveDials(effortModel(), types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, layers...)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("not deterministic:\n%+v\n%+v", a, b)
	}
	if decision(t, a, types.DialReasoning).Scope != types.DialScopeTurn || decision(t, a, types.DialMaxOutput).Scope != types.DialScopePreset {
		t.Fatalf("scopes: %+v", a.Decisions)
	}
	if a.EffectiveHash == "" || a.Changed() {
		t.Fatalf("hash or changed: %+v", a)
	}
}

func TestDialReportJSONRoundTrip(t *testing.T) {
	_, rep := resolve(t, effortModel(), types.RequestOptions{}, types.StrictDials.Clone(), types.Dials{Reasoning: depth(types.DepthHigh)})
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back types.DialReport
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep, back) {
		t.Fatalf("round trip:\n%+v\n%+v\n%s", rep, back, data)
	}
}
