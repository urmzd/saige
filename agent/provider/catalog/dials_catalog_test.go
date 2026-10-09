package catalog

import (
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// dialRecord is one compiled (model, dials) pair in the dials golden file.
type dialRecord struct {
	Provider  string               `json:"provider"`
	Model     string               `json:"model"`
	Dials     string               `json:"dials"`
	Decisions []types.DialDecision `json:"decisions,omitempty"`
	Error     string               `json:"error,omitempty"`
}

// TestDefaultDialsGolden freezes what every current default row compiles
// each dial value to, so a change to a row or to the derived mapping is a reviewed
// diff (go test -run TestDefaultDialsGolden -update).
func TestDefaultDialsGolden(t *testing.T) {
	var out []dialRecord
	for _, e := range Default().view().entries {
		mc := e.Caps.ForModel(e.Prefix)
		mc.Provider = e.Provider
		if e.SupersededBy != "" || (mc.Supports(types.CapEmbeddings) && !mc.Supports(types.CapStreaming)) {
			continue
		}
		for _, d := range allDialValues() {
			label := strings.Join(dialNames(d), ",")
			_, rep, err := types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{},
				types.DialLayer{Scope: types.DialScopeEntry, Dials: d})
			r := dialRecord{Provider: e.Provider, Model: e.Prefix, Dials: label + "=" + rep.Decisions[0].Requested, Decisions: rep.Decisions}
			for i := range r.Decisions {
				r.Decisions[i].Scope = ""
			}
			if err != nil {
				r.Error = types.OptionReason(err)
			}
			out = append(out, r)
		}
	}
	checkGolden(t, "dials_golden.json", jsonLines(t, out))
}

func dialNames(d types.Dials) []string {
	var out []string
	for _, n := range d.Names() {
		out = append(out, string(n))
	}
	return out
}

// Scenario (h): an overlay patches one creativity level of one row with a
// JSON merge patch. The other levels stay derived, and the entry's
// ConfigHash follows the compiled mapping.
func TestOverlayPatchesDialMapping(t *testing.T) {
	const preset = `"presets":{"p":{"dials":{"creativity":"focused"},"chain":[{"provider":"openai","model":"gpt-4.1"}]}}`
	base := mustOverlay(t, `{"version":1,`+preset+`}`)
	patched := mustOverlay(t, `{"version":1,"models":[{"provider":"openai","prefix":"gpt-4.1",
		"dials":{"creativity":{"levels":{"focused":{"temperature":0.2}}}}}],`+preset+`}`)
	mc, _ := patched.Lookup("openai", "gpt-4.1")
	eff, _, err := types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{},
		types.DialLayer{Dials: types.Dials{Creativity: dialCreativity(types.CreativityFocused)}})
	if err != nil || *eff.Temperature != 0.2 || eff.TopP != nil {
		t.Fatalf("patched level: %v %+v", err, eff)
	}
	eff, _, _ = types.ResolveDials(mc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{},
		types.DialLayer{Dials: types.Dials{Creativity: dialCreativity(types.CreativityCreative)}})
	if *eff.Temperature != 1 {
		t.Fatalf("an unpatched level lost its derived value: %+v", eff)
	}
	a, err := base.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	b, err := patched.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	if a.Chain[0].ConfigHash == b.Chain[0].ConfigHash {
		t.Fatal("the ConfigHash does not cover the compiled mapping")
	}
}

// Scenario (h): a mapping the row cannot send fails when the catalog loads.
func TestBadDialMappingFailsAtLoad(t *testing.T) {
	for _, tc := range []struct{ name, row, path string }{
		{"unknown effort", `{"provider":"openai","prefix":"gpt-6.1-sol","dials":{"reasoning":{"depth":{"high":{"reasoning":{"effort":"ultra"}}}}}}`,
			"models[%d].dials.reasoning.depth.high"},
		{"unsupported sampling", `{"provider":"anthropic","prefix":"claude-haiku-5-5","dials":{"creativity":{"levels":{"focused":{"temperature":0.3}}}}}`,
			"models[%d].dials.creativity.levels.focused"},
		{"inexpressible control", `{"provider":"ollama","prefix":"qwen3","dials":{"reasoning":{"on":{"reasoning":{"effort":"high"}}}}}`,
			"models[%d].dials.reasoning.on"},
		{"unknown level", `{"provider":"openai","prefix":"gpt-4.1","dials":{"creativity":{"levels":{"wild":{"temperature":2}}}}}`,
			"models[%d].dials.creativity.levels.wild"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer, err := Load(strings.NewReader(`{"version":1,"models":[` + tc.row + `]}`))
			if err == nil {
				_, err = Merge(Default(), layer)
			}
			var ve *ValidationError
			if err == nil || !asValidation(err, &ve) {
				t.Fatalf("loaded: %v", err)
			}
			prefix, suffix, _ := strings.Cut(tc.path, "%d")
			if !slices.ContainsFunc(ve.Issues, func(is Issue) bool {
				return is.Code == CodeDial && strings.HasPrefix(is.Path, prefix) && strings.HasSuffix(is.Path, suffix)
			}) {
				t.Fatalf("no %s issue at %s: %v", CodeDial, tc.path, ve.Issues)
			}
		})
	}
}

func asValidation(err error, ve **ValidationError) bool {
	v, ok := err.(*ValidationError)
	if ok {
		*ve = v
	}
	return ok
}

// Scenario (a) in a preset: creativity is advisory, so a chain whose
// members cannot take it still resolves, with a warning per entry.
func TestPresetDialsResolvePerEntry(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"presets":{"p":{"dials":{"creativity":"focused","reasoning":{"depth":"high"}},"chain":[
		{"provider":"openai","model":"gpt-6-luna"},{"provider":"anthropic","model":"claude-haiku-5-5"},
		{"provider":"ollama","model":"qwen3","unset":["dials.creativity"]}]}}}`)
	rp, err := c.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	if got := mergedDials(rp.Chain[2].Dials); got.Creativity != nil || got.Reasoning == nil {
		t.Fatalf("unset dials.creativity: %+v", got)
	}
	if rp.Chain[0].DialOrigin[types.DialCreativity] != LayerPreset {
		t.Fatalf("origin: %+v", rp.Chain[0].DialOrigin)
	}
	want := map[string]string{
		"presets.p.chain[1].dials.creativity": WarnDialDropped,
		"presets.p.chain[2].dials.reasoning":  WarnDialMapped,
	}
	for path, code := range want {
		if !slices.ContainsFunc(rp.Warnings, func(is Issue) bool { return is.Path == path && is.Code == code }) {
			t.Errorf("no %s warning at %s: %v", code, path, rp.Warnings)
		}
	}
}

// Scenario (b): a seed is contractual, so a preset that holds one fails on
// an entry that cannot take it, naming the layer.
func TestPresetContractualDialFails(t *testing.T) {
	_, err := overlay(t, `{"version":1,"presets":{"p":{"dials":{"reproducible":7},"chain":[
		{"provider":"openai","model":"gpt-4.1"},{"provider":"anthropic","model":"claude-haiku-5-5"}]}}}`)
	issueAt(t, err, "presets.p.chain[1].dials.reproducible", CodeDial)
	if !strings.Contains(err.Error(), "inherited from preset options") {
		t.Fatalf("message does not name the layer: %v", err)
	}
	// An entry that opts out of the preset is not held to it.
	mustOverlay(t, `{"version":1,"presets":{"p":{"dials":{"reproducible":7},"chain":[
		{"provider":"openai","model":"gpt-4.1"},{"provider":"anthropic","model":"claude-haiku-5-5","unset":["dials.reproducible"]}]}}}`)
}

func TestCacheDialSelectsRowPromptCache(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"dials":{"cache":true},"presets":{"p":{"chain":[
		{"provider":"anthropic","model":"claude-haiku-5-5"},{"provider":"openai","model":"gpt-6-luna"},
		{"provider":"openai","model":"gpt-4.1","options":{"prompt_cache":{"mode":"off"}}}]}}}`)
	rp, err := c.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	if pc := rp.Chain[0].PromptCache; pc == nil || pc.Mode != PromptCacheMarkers {
		t.Fatalf("anthropic cache: %+v", pc)
	}
	if pc := rp.Chain[1].PromptCache; pc == nil || pc.Mode != PromptCacheAutomatic || rp.Chain[1].Origin[optionPromptCache] != LayerCatalog {
		t.Fatalf("openai cache: %+v %v", pc, rp.Chain[1].Origin)
	}
	if pc := rp.Chain[2].PromptCache; pc == nil || pc.Mode != PromptCacheOff {
		t.Fatalf("an explicit prompt_cache must win: %+v", pc)
	}
}

func TestPresetRawSamplingSuggestsDial(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"presets":{"p":{"options":{"max_output_tokens":100},"chain":[
		{"provider":"openai","model":"gpt-4.1"},{"provider":"openai","model":"gpt-4o"}]},
		"q":{"options":{"temperature":0.2},"chain":[{"provider":"openai","model":"gpt-4.1"},{"provider":"openai","model":"gpt-4o"}]}}}`)
	found := c.Issues()
	if !slices.ContainsFunc(found, func(is Issue) bool { return is.Path == "presets.q.options.temperature" && is.Code == WarnPreferDial }) {
		t.Fatalf("no prefer_dial warning: %v", found)
	}
	if slices.ContainsFunc(found, func(is Issue) bool { return strings.HasPrefix(is.Path, "presets.p.") && is.Code == WarnPreferDial }) {
		t.Fatalf("max_output_tokens is not vendor specific: %v", found)
	}
}

func TestDialsSurviveExportAndCanonicalRoundTrip(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"models":[{"provider":"openai","prefix":"gpt-4.1",
		"dials":{"reasoning":{"depth_change":"per_request"},"defaults":{"creativity":"balanced"}}}]}`)
	data, err := c.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Load(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := back.Describe("openai", "gpt-4.1")
	if !ok || e.Dials == nil || e.Dials.Defaults == nil || *e.Dials.Defaults.Creativity != types.CreativityBalanced {
		t.Fatalf("dials lost: %+v", e.Dials)
	}
	if e.Caps.DialMap.Reasoning == nil || e.Caps.DialMap.Reasoning.DepthChange != types.DepthChangePerRequest {
		t.Fatalf("dial map: %+v", e.Caps.DialMap)
	}
	if got := entrySpec(e).Dials; got == nil || got.Defaults == nil {
		t.Fatalf("export lost dials: %+v", got)
	}
}
