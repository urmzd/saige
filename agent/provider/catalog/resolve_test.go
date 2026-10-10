package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// overlay loads a layer and merges it onto the embedded default.
func overlay(t *testing.T, doc string) (*Catalog, error) {
	t.Helper()
	layer, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return Merge(Default(), layer)
}

func mustOverlay(t *testing.T, doc string) *Catalog {
	t.Helper()
	c, err := overlay(t, doc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPresetRejectsUnsupportedInherited(t *testing.T) {
	const doc = `{"version":1,"presets":{"p":{"options":{"temperature":0.2},"chain":[
		{"provider":"openai","model":"gpt-4.1"},
		{"provider":"openai","model":"o3"%s}]}}}`
	_, err := overlay(t, strings.Replace(doc, "%s", "", 1))
	issueAt(t, err, "presets.p.chain[1].options.temperature", CodeUnsupported)
	if !strings.Contains(err.Error(), "inherited from preset options") {
		t.Fatalf("message does not name the layer: %v", err)
	}
	c := mustOverlay(t, strings.Replace(doc, "%s", `,"unset":["temperature"]`, 1))
	rp, err := c.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Chain[1].Options.Temperature != nil || *rp.Chain[0].Options.Temperature != 0.2 {
		t.Fatalf("unset applied to the wrong entry: %+v", rp.Chain)
	}
}

func TestPresetRejectsInexpressibleOption(t *testing.T) {
	_, err := overlay(t, `{"version":1,"presets":{"p":{"options":{"seed":3},"chain":[{"provider":"anthropic","model":"claude-3-5-haiku"}]}}}`)
	issueAt(t, err, "presets.p.chain[0].options.seed", CodeUnsupported)
}

func TestOutputModeNativeNeedsNativeEntries(t *testing.T) {
	_, err := overlay(t, `{"version":1,"presets":{"p":{"output_mode":"native","chain":[{"provider":"anthropic","model":"claude-3-5-haiku"}]}}}`)
	issueAt(t, err, "presets.p.chain[0]", CodeOutputMode)
	if !strings.Contains(err.Error(), `use "output_mode": "tool"`) {
		t.Fatalf("no hint: %v", err)
	}
}

func TestPrecedence(t *testing.T) {
	c := mustOverlay(t, `{"version":1,
		"models":[{"provider":"openai","prefix":"gpt-4.1","defaults":{"max_output_tokens":1000,"seed":1,"temperature":0.1}}],
		"presets":{"p":{"options":{"temperature":0.5,"seed":2,"stop":["x"]},"chain":[
			{"id":"a","provider":"openai","model":"gpt-4.1","options":{"temperature":0.9},"unset":["stop"]},
			{"id":"b","provider":"openai","model":"gpt-4.1","inherit":"none"},
			{"id":"c","provider":"openai","model":"gpt-5.2","options":{"reasoning":{"effort":"high"}},"inherit":"none","unset":["max_output_tokens","seed","temperature"]},
			{"id":"d","provider":"anthropic","model":"claude-sonnet-4-6","inherit":"none"}]}}}`)
	rp, err := c.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	a, b, cc, d := rp.Chain[0], rp.Chain[1], rp.Chain[2], rp.Chain[3]
	if *a.Options.Temperature != 0.9 || *a.Options.Seed != 2 || *a.Options.MaxOutputTokens != 1000 || a.Options.StopSequences != nil {
		t.Fatalf("a: %+v", a.Options)
	}
	if a.Origin["temperature"] != LayerEntry || a.Origin["seed"] != LayerPreset || a.Origin["max_output_tokens"] != LayerModel {
		t.Fatalf("a origin: %v", a.Origin)
	}
	if _, ok := a.Origin["stop"]; ok {
		t.Fatal("unset option keeps an origin")
	}
	if *b.Options.Temperature != 0.1 || *b.Options.Seed != 1 || b.Origin["temperature"] != LayerModel {
		t.Fatalf("inherit none must keep model defaults only: %+v %v", b.Options, b.Origin)
	}
	if cc.Options.ReasoningEffort == nil || cc.Options.Temperature != nil {
		t.Fatalf("c: %+v", cc.Options)
	}
	if d.Origin["max_output_tokens"] != LayerProvider || d.Options.MaxOutputTokens != nil {
		t.Fatalf("anthropic's default max_tokens is a provider default: %v", d.Origin)
	}
	if a.ProfileID != "p/a" {
		t.Fatalf("profile id %q", a.ProfileID)
	}
}

func TestReasoningReplacesWhole(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"presets":{"p":{"options":{"reasoning":{"budget":2048}},"chain":[
		{"provider":"anthropic","model":"claude-sonnet-4-6","options":{"reasoning":{"effort":"low"}}}]}}}`)
	rp, err := c.Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	o := rp.Chain[0].Options
	if o.ReasoningBudget != nil || o.ReasoningEffort == nil || *o.ReasoningEffort != "low" {
		t.Fatalf("reasoning merged field by field: %+v", o)
	}
}

func TestUnsetOfOwnOptionIsAnError(t *testing.T) {
	_, err := overlay(t, `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","options":{"seed":1},"unset":["seed"]}]}}}`)
	issueAt(t, err, "presets.p.chain[0].unset[0]", CodeUnset)
}

func TestRequireDeclared(t *testing.T) {
	_, err := overlay(t, `{"version":1,"presets":{"p":{"require_declared":true,"chain":[{"provider":"openai","model":"gpt-4.1-2099"}]}}}`)
	issueAt(t, err, "presets.p.chain[0].model", CodeNotDeclared)
	c := mustOverlay(t, `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1-2099"}]}}}`)
	rp, err := c.Resolve("p")
	if err != nil || len(rp.Warnings) == 0 || rp.Warnings[0].Code != WarnInferredModel {
		t.Fatalf("inferred model must warn: %v %v", err, rp.Warnings)
	}
}

func TestPresetExtends(t *testing.T) {
	c := loadTestdata(t, "full.json")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve("deterministic-extract"); err != nil {
		t.Fatal(err)
	}
	strict := c.clone()
	_, err := strict.Resolve("deterministic-extract-strict")
	if err != nil {
		t.Fatalf("both models are exact rows: %v", err)
	}
}

func TestMergeModelsFieldLevel(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"revision":"local","models":[
		{"provider":"openai","prefix":"gpt-4o","pricing":{"currency":"USD","input_per_mtok":1,"output_per_mtok":2,"as_of":"2026-10-01"}},
		{"provider":"openai","prefix":"gpt-4o-mini","limits":null,"add_capabilities":["reasoning"]},
		{"provider":"openai","prefix":"o1","$delete":true},
		{"provider":"openai","prefix":"gpt-4-turbo","$replace":true,"capabilities":["streaming"]}]}`)
	before, _ := Default().Lookup("openai", "gpt-4o")
	after, _ := c.Lookup("openai", "gpt-4o")
	// Nested objects merge per RFC 7396: the cached rate stays.
	if after.Pricing.InputPerMTok != 1 || after.Pricing.CachedInputPerMTok != before.Pricing.CachedInputPerMTok {
		t.Fatalf("pricing patch: %+v", after.Pricing)
	}
	after.Pricing, before.Pricing = types.Pricing{}, types.Pricing{}
	if after.ContextWindow != before.ContextWindow || len(after.Caps) != len(before.Caps) {
		t.Fatal("a price change moved the capabilities")
	}
	mini, _ := c.Lookup("openai", "gpt-4o-mini")
	if mini.ContextWindow != 0 || !mini.Supports(types.CapReasoning) || !mini.Supports(types.CapTools) {
		t.Fatalf("null delete or capability edit lost: %+v", mini)
	}
	if e, _ := c.Describe("openai", "o1"); e.Prefix == "o1" {
		t.Fatal("$delete kept the row")
	}
	turbo, _ := c.Lookup("openai", "gpt-4-turbo")
	if len(turbo.List()) != 1 || turbo.Pricing.InputPerMTok != 0 {
		t.Fatalf("$replace merged: %v", turbo.List())
	}
	if !strings.HasSuffix(c.Revision, "+local") {
		t.Fatalf("revision %q", c.Revision)
	}
}

func TestMergePresetsEntryLevel(t *testing.T) {
	base := loadTestdata(t, "full.json")
	layer, err := Load(strings.NewReader(`{"version":1,"revision":"over","presets":{
		"balanced":{"chain":[{"provider":"openai","model":"gpt-4.1"}]},
		"balanced-cold":{"extends":"balanced","options":{"temperature":0}},
		"deterministic-extract-strict":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Merge(base, layer)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Presets["balanced"]; len(p.Chain) != 1 || p.Options != nil || p.Description != "" {
		t.Fatalf("overlay preset must replace whole: %+v", p)
	}
	cold, err := c.Resolve("balanced-cold")
	if err != nil || *cold.Chain[0].Options.Temperature != 0 {
		t.Fatalf("extends: %v %+v", err, cold)
	}
	if _, ok := c.Presets["deterministic-extract-strict"]; ok {
		t.Fatal("null did not delete the preset")
	}
	if c.Revision != "test.1+over" {
		t.Fatalf("revision %q", c.Revision)
	}
}

func TestInheritDefaultFalseDropsLowerLayers(t *testing.T) {
	c, err := Merge(Default(), loadTestdata(t, "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Describe("openai", "o3"); ok {
		t.Fatal("a standalone layer kept rows below it")
	}
}

func TestInstallRevisions(t *testing.T) {
	const prefix = "install-test"
	base := mustOverlay(t, `{"version":1,"models":[{"provider":"openai","prefix":"install-test","extends":"openai.chat","pricing":{"input_per_mtok":1,"as_of":"2026-10-01"}}]}`)
	if _, err := Install(base, registry.WithSource("first")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = Install(Default()) })
	n := Revisions("openai", prefix)
	rep, err := Install(base, registry.WithSource("again"))
	if err != nil {
		t.Fatal(err)
	}
	if Revisions("openai", prefix) != n || len(rep.Changed) != 0 {
		t.Fatalf("unchanged install added revisions: %+v", rep)
	}
	if err := Pin("openai", prefix, registry.Revision(n)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Unpin("openai", prefix) })
	changed := mustOverlay(t, `{"version":1,"models":[{"provider":"openai","prefix":"install-test","extends":"openai.chat","pricing":{"input_per_mtok":2,"as_of":"2026-10-01"}}]}`)
	rep, err = Install(changed, registry.WithSource("changed"))
	if err != nil {
		t.Fatal(err)
	}
	h := History("openai", prefix)
	if len(h) != n+1 || h[len(h)-1].Source != "changed" || len(rep.Pinned) != 1 {
		t.Fatalf("history %+v report %+v", h, rep)
	}
	if c, _ := Lookup("openai", prefix); c.Pricing.InputPerMTok != 1 {
		t.Fatal("pin did not survive Install")
	}
	Unpin("openai", prefix)
	rep, err = Install(Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Describe("openai", prefix); ok || len(rep.Removed) != 1 {
		t.Fatalf("dropped row still resolves: %+v", rep)
	}
	if Active().Revision != Default().Revision {
		t.Fatal("Active does not track Install")
	}
}

func TestConfigHashStable(t *testing.T) {
	doc := `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","api_key_env":"MY_KEY","options":{"seed":1,"temperature":0.5}}]}}}`
	reordered := `{"presets":{"p":{"chain":[{"options":{"temperature":0.5,"seed":1},"api_key_env":"MY_KEY","model":"gpt-4.1","provider":"openai"}]}},"version":1}`
	h1 := resolvedHash(t, doc)
	if h2 := resolvedHash(t, reordered); h1 != h2 {
		t.Fatalf("hash depends on key order: %s %s", h1, h2)
	}
	if h3 := resolvedHash(t, strings.Replace(doc, `"seed":1`, `"seed":2`, 1)); h3 == h1 {
		t.Fatal("hash ignores an option change")
	}
	if len(h1) != 16 {
		t.Fatalf("hash %q", h1)
	}
	t.Setenv("MY_KEY", "sk-secret")
	if resolvedHash(t, doc) != h1 {
		t.Fatal("hash depends on the key's value")
	}
}

func resolvedHash(t *testing.T, doc string) string {
	t.Helper()
	rp, err := mustOverlay(t, doc).Resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	return rp.Chain[0].ConfigHash
}

func TestResolveModel(t *testing.T) {
	rp, err := Default().ResolveModel("", "gpt-4.1")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Chain[0].Provider != "openai" || rp.Chain[0].ProfileID != "openai/gpt-4.1" {
		t.Fatalf("%+v", rp.Chain[0])
	}
	if _, err := Default().ResolveModel("", "no-such-model"); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("got %v", err)
	}
}

func TestExpressibleSharedTable(t *testing.T) {
	seed := int64(1)
	var ee *ExpressError
	if err := Expressible("anthropic", types.RequestOptions{Seed: &seed}); !errors.As(err, &ee) || ee.Option != "seed" {
		t.Fatalf("got %v", err)
	}
	if err := Expressible("openai", types.RequestOptions{Seed: &seed}); err != nil {
		t.Fatal(err)
	}
	if ExpressiblePromptCache("google", PromptCacheMarkers) == nil || ExpressiblePromptCache("anthropic", PromptCacheMarkers) != nil {
		t.Fatal("prompt cache table")
	}
}

func TestPresetCompaction(t *testing.T) {
	c := mustOverlay(t, `{"version":1,"presets":{"base":{"compaction":{"strategy":"chain","max_input_tokens":50000,"chain":[
			{"strategy":"clear_tool_results","keep_tool_results":2},
			{"strategy":"relevant_plus_summary","keep_turns":3,"select_k":2,"summary_model":"claude-haiku-5-5"}]},
		"chain":[{"provider":"anthropic","model":"claude-haiku-5-5"}]},
		"child":{"extends":"base"}}}`)
	for _, name := range []types.PresetName{"base", "child"} {
		rp, err := c.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		cc := rp.Compaction
		if cc == nil || cc.Strategy != types.CompactChain || cc.MaxInputTokens != 50000 || len(cc.Chain) != 2 {
			t.Fatalf("%s: compaction = %+v", name, cc)
		}
		if s := cc.Chain[1]; s.Strategy != types.CompactRelevantPlusSummary || s.KeepTurns != 3 || s.SelectK != 2 || s.SummaryModel != "claude-haiku-5-5" {
			t.Fatalf("%s: step = %+v", name, s)
		}
	}
}

func TestPresetCompactionIsValidated(t *testing.T) {
	tests := []struct {
		spec, path string
	}{
		{`{"strategy":"shrink"}`, "presets.p.compaction.strategy"},
		{`{"strategy":"chain"}`, "presets.p.compaction.chain"},
		{`{"strategy":"summary","chain":[{"strategy":"keep_recent"}]}`, "presets.p.compaction.chain"},
		{`{"strategy":"chain","chain":[{"strategy":"keep_recent","keep_turns":-1}]}`, "presets.p.compaction.chain[0].keep_turns"},
	}
	for _, tt := range tests {
		_, err := Load(strings.NewReader(`{"version":1,"presets":{"p":{"compaction":` + tt.spec + `,"chain":[{"provider":"anthropic","model":"claude-haiku-5-5"}]}}}`))
		issueAt(t, err, tt.path, CodeBadValue)
	}
}
