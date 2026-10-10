package catalog

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func mustLoad(t *testing.T, doc string) *Catalog {
	t.Helper()
	c, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestDefaultOfferingsProject checks every default model: its offering on
// the primary endpoint projects to what Lookup returns, its parameter
// space rejects what request validation rejects, and its dial map derived
// from the space equals the one derived from the projection.
func TestDefaultOfferingsProject(t *testing.T) {
	c := Default()
	opts := []types.RequestOptions{
		{}, {Temperature: ptr(0.5)}, {Temperature: ptr(3.0)}, {TopP: ptr(0.9)}, {TopK: ptr(40.0)}, {Seed: ptr(int64(1))},
		{MaxOutputTokens: ptr(int64(100))}, {MaxOutputTokens: ptr(int64(10_000_000))}, {StopSequences: []string{"x"}},
		{ParallelTools: ptr(false)}, {ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}},
		{ReasoningEffort: ptr("none")}, {ReasoningEffort: ptr("low")}, {ReasoningEffort: ptr("max")},
		{ReasoningEffort: ptr("none"), Temperature: ptr(1.0)}, {ReasoningEffort: ptr("high"), Temperature: ptr(1.0)},
		{ReasoningBudget: ptr(int64(2048))}, {ReasoningBudget: ptr(int64(0))}, {ReasoningBudget: ptr(int64(-1))},
		{ReasoningBudget: ptr(int64(0)), Temperature: ptr(0.2)}, {ReasoningEnabled: ptr(true)}, {ReasoningEnabled: ptr(false)},
		{ReasoningEnabled: ptr(false), Temperature: ptr(0.2)},
	}
	v := c.view()
	for _, e := range v.entries {
		name := modelKey(e.Provider, e.Prefix)
		primary, _ := c.PrimaryEndpoint(e.Provider)
		off, ok := e.Offerings[primary]
		if !ok {
			t.Errorf("%s has no offering on %s", name, primary)
			continue
		}
		want, _ := c.Lookup(e.Provider, e.Prefix)
		got := off.Capabilities()
		got.Provider, got.Model, got.Family, got.Known = want.Provider, want.Model, want.Family, want.Known
		got.Offering, want.Offering = nil, nil
		if !reflect.DeepEqual(got.ForModel(got.Model), want.ForModel(want.Model)) {
			t.Errorf("%s: projection differs from Lookup", name)
		}
		space := off.Space()
		for i, o := range opts {
			w := want.ValidateOptions(o)
			g := space.Validate(o, types.RequestShape{Provider: want.Provider, Model: want.Model})
			if fmt.Sprint(g) != fmt.Sprint(w) {
				t.Errorf("%s option %d: space %v, validation %v", name, i, g, w)
			}
		}
		if !reflect.DeepEqual(off.EffectiveDialMap(), want.EffectiveDialMap()) {
			t.Errorf("%s: dial map from the space differs from the projection's", name)
		}
	}
}

// TestImplicitOfferings covers the offerings the catalog synthesizes: an
// endpoint that inherits another's, a default offering template for a
// model with no offering, endpoint overrides, and a batch tier dropped on
// an endpoint without a batch mode.
func TestImplicitOfferings(t *testing.T) {
	c := Default()
	for _, e := range c.view().entries {
		var inherit string
		switch e.Provider {
		case providerOpenAI:
			inherit = EndpointOpenAIResponses
		case providerGoogle:
			inherit = EndpointGoogleVertex
		default:
			continue
		}
		if _, ok := e.Offerings[inherit]; !ok {
			t.Errorf("%s/%s has no inherited offering on %s", e.Provider, e.Prefix, inherit)
		}
	}
	resp, ok := c.Offering(EndpointOpenAIResponses, "openai", "gpt-6-luna")
	if !ok || resp.Endpoint.Surface != types.SurfaceOpenAIResponses || resp.ID != "openai/gpt-6-luna@openai-responses" {
		t.Fatalf("responses offering = %+v", resp.Endpoint)
	}
	if _, ok := resp.Tiers[types.ServiceBatch]; !ok {
		t.Error("the batch tier was not inherited onto an endpoint with a batch mode")
	}

	doc := `{"version":2,"inherit_default":false,
	  "models":{"acme/rocket-1":{"limits":{"context_window":1000}}, "acme/rocket-2":{}},
	  "endpoints":{
	    "acme":{"surface":"openai.compatible","serves":["acme"],"primary":true,"default_offering_template":"acme.base","modes":{"batch":true}},
	    "acme-eu":{"surface":"openai.compatible","serves":["acme"],"inherit_offerings":"acme",
	      "overrides":{"remove_features":["tools"],"params":{"temperature":{"max":1}}}}},
	  "offering_templates":{"acme.base":{"features":["tools","streaming"],"params":{"temperature":{"type":"number"}},
	    "pricing":{"input_per_mtok":1,"output_per_mtok":2,"as_of":"2026-10-09"},
	    "tiers":{"batch":{"transport":"batch","discount":0.5}}}},
	  "offerings":[{"model":"acme/rocket-1","endpoint":"acme","extends":"acme.base","add_features":["structured_output"]}]}`
	cat := mustLoad(t, doc)
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	r1, _ := cat.Lookup("acme", "rocket-1")
	if !r1.Supports(types.CapStructuredOutput) || !r1.Supports(types.CapTemperature) || r1.ContextWindow != 1000 {
		t.Errorf("explicit offering = %v", r1.List())
	}
	r2, known := cat.Lookup("acme", "rocket-2")
	if !known || !r2.Supports(types.CapTools) || r2.Supports(types.CapStructuredOutput) {
		t.Errorf("synthesized offering = %v (known %v)", r2.List(), known)
	}
	eu, ok := cat.Offering("acme-eu", "acme", "rocket-1")
	if !ok || slices.Contains(eu.Features, types.CapTools) || *eu.Params.Params[types.ParamTemperature].Max != 1 {
		t.Fatalf("override not applied: %+v", eu.Features)
	}
	if _, ok := eu.Tiers[types.ServiceBatch]; ok {
		t.Error("batch tier kept on an endpoint without a batch mode")
	}
	base, known := cat.Lookup("acme", "unlisted")
	if known || !base.Supports(types.CapTools) || base.Offering == nil || base.Offering.Model.Known {
		t.Errorf("baseline = %v known %v", base.List(), known)
	}
}

func TestBatchTierNeedsBatchMode(t *testing.T) {
	_, err := Load(strings.NewReader(`{"version":2,"inherit_default":false,"models":{"acme/m":{}},
	  "endpoints":{"acme":{"surface":"openai.compatible","serves":["acme"]}},
	  "offerings":[{"model":"acme/m","endpoint":"acme","features":["streaming"],
	    "pricing":{"input_per_mtok":1,"as_of":"2026-10-09"},"tiers":{"batch":{"transport":"batch","discount":0.5}}}]}`))
	issueAt(t, err, "offerings[0].tiers.batch", CodeTier)
}

func TestEndpointSecretMustBeAReference(t *testing.T) {
	_, err := Load(strings.NewReader(`{"version":2,"endpoints":{"x":{"surface":"openai.chat","serves":["openai"],"auth":{"secret":"env:MY_KEY"}}}}`))
	if err != nil {
		t.Fatalf("an env reference was refused: %v", err)
	}
}

// TestChainEntryForms resolves each way a chain entry names its target.
func TestChainEntryForms(t *testing.T) {
	cat := mustOverlay(t, `{"version":2,"presets":{"forms":{"chain":[
		{"id":"legacy","provider":"openai","model":"gpt-6-luna"},
		{"id":"offering","offering":"openai/gpt-6-luna@openai-responses"},
		{"id":"endpoint","endpoint":"openai-responses","model":"gpt-6-luna"},
		{"id":"vertex","provider":"google","model":"gemini-3.1-flash-lite","vertex":{"project":"p"}},
		{"id":"gateway","provider":"openai","model":"gpt-6-luna","base_url":"https://gw.example","api_key_env":"GW_KEY"},
		{"id":"vertex-endpoint","endpoint":"google-vertex","model":"gemini-3.1-flash-lite"}]}}}`)
	rp, err := cat.Resolve("forms")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ResolvedEntry{}
	for _, e := range rp.Chain {
		by[e.ID] = e
	}
	for id, want := range map[string]string{"legacy": EndpointOpenAIChat, "offering": EndpointOpenAIResponses,
		"endpoint": EndpointOpenAIResponses, "vertex": EndpointGoogleVertex, "gateway": "forms/gateway", "vertex-endpoint": EndpointGoogleVertex} {
		if got := by[id].Endpoint; got != want {
			t.Errorf("%s: endpoint %q, want %q", id, got, want)
		}
	}
	if by["offering"].Provider != "openai" || by["offering"].Model != "gpt-6-luna" || by["offering"].Offering != "openai/gpt-6-luna@openai-responses" {
		t.Errorf("offering entry = %+v", by["offering"])
	}
	if by["legacy"].Offering != "openai/gpt-6-luna@openai-chat" {
		t.Errorf("legacy offering = %q", by["legacy"].Offering)
	}
	if by["offering"].ConfigHash == by["legacy"].ConfigHash || by["offering"].ConfigHash != by["endpoint"].ConfigHash {
		t.Error("the config hash does not tell the endpoints apart, or the two spellings of one offering differ")
	}
	if v := by["vertex-endpoint"].Vertex; v == nil || v.Project != "" {
		t.Errorf("vertex endpoint placement = %+v", v)
	}
	if by["gateway"].BaseURL != "https://gw.example" || by["gateway"].APIKeyEnv != "GW_KEY" {
		t.Errorf("gateway = %+v", by["gateway"])
	}
	if by["legacy"].APIKeyEnv != "" {
		t.Errorf("the default key variable became an override: %q", by["legacy"].APIKeyEnv)
	}

	_, err = overlay(t, `{"version":2,"presets":{"bad":{"chain":[
		{"id":"a","offering":"openai/gpt-6-luna@nowhere"},
		{"id":"b","provider":"anthropic","offering":"openai/gpt-6-luna@openai-chat"},
		{"id":"c","offering":"no-at-sign"}]}}}`)
	issueAt(t, err, "presets.bad.chain[0].offering", CodeUnknownEndpoint)
	issueAt(t, err, "presets.bad.chain[1].provider", CodeBadValue)
	issueAt(t, err, "presets.bad.chain[2].offering", CodeBadValue)
}

// TestPromptCacheRetention checks the offering's retention rule reaches a
// preset entry: the GPT-5.6 and later rows keep prompt caches for 24 hours
// only (the API rejects in_memory), and earlier rows take both.
func TestPromptCacheRetention(t *testing.T) {
	for _, tc := range []struct {
		model, retention string
		ok               bool
	}{
		{"gpt-6-luna", "in_memory", false}, {"gpt-6-luna", "24h", true},
		{"gpt-6.1-sol", "in_memory", false}, {"gpt-4.1", "in_memory", true},
		{"gpt-6-sol", "in_memory", false}, {"gpt-6-sol", "24h", true},
		{"gpt-6-astra", "in_memory", false}, {"gpt-6-astra", "24h", true},
		{"gpt-5.6-luna", "in_memory", false}, {"gpt-5.6-sol", "in_memory", false}, {"gpt-5.6-terra", "in_memory", false},
		{"gpt-5.2", "in_memory", true}, {"gpt-5.2", "24h", true},
	} {
		_, err := overlay(t, fmt.Sprintf(`{"version":2,"presets":{"p":{"chain":[{"provider":"openai","model":%q,
			"options":{"prompt_cache":{"mode":"automatic","retention":%q}}}]}}}`, tc.model, tc.retention))
		if tc.ok && err != nil {
			t.Errorf("%s %s: %v", tc.model, tc.retention, err)
		}
		if !tc.ok {
			issueAt(t, err, "presets.p.chain[0].options.prompt_cache", CodePromptCache)
		}
	}
	caps := MustLookup("openai", "gpt-6-luna")
	if caps.Offering == nil {
		t.Fatal("Lookup carries no offering")
	}
	if err := caps.Offering.AcceptsValue(types.ParamPromptCacheRetention, "in_memory"); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Errorf("in_memory accepted: %v", err)
	}
}

// TestDefaultModalities checks a few declared modality facts.
func TestDefaultModalities(t *testing.T) {
	q := MustLookup("ollama", "qwen3.5:4b")
	if !q.Media.NativeTypes[types.MediaPNG] || q.Offering == nil {
		t.Fatalf("qwen3.5 media = %v", q.Media.NativeTypes)
	}
	if l := q.Offering.Modalities.In[types.ModalityImage]; !slices.Equal(l.Sources, []types.SourceKind{types.SourceInline}) {
		t.Errorf("qwen3.5 image sources = %v", l.Sources)
	}
	a := MustLookup("anthropic", "claude-haiku-5-5")
	if l, ok := a.Offering.Modalities.Accepts(types.MediaPDF); !ok || l.MaxBytes != 32<<20 {
		t.Errorf("anthropic pdf = %+v %v", l, ok)
	}
	g := MustLookup("google", "gemini-3.1-flash-lite")
	if l := g.Offering.Modalities.In[types.ModalityVideo]; l.FPS == nil || l.FPS.Default != 1 {
		t.Errorf("gemini video fps = %+v", l.FPS)
	}
}

func TestUpgradeV1KeepsPresetsAndWarns(t *testing.T) {
	c := mustLoad(t, `{"version":1,"models":[{"provider":"openai","prefix":"x","extends":"openai.chat"}],
		"presets":{"p":{"chain":[{"provider":"openai","model":"x"}]}}}`)
	if c.Version != SchemaVersion || len(c.Offerings) != 1 || c.Offerings[0].Endpoint != EndpointOpenAIChat {
		t.Fatalf("upgraded = %+v", c.Offerings)
	}
	merged, err := Merge(Default(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(merged.Issues(), func(is Issue) bool { return is.Code == WarnV1Catalog }) {
		t.Error("no upgrade warning")
	}
	if _, err := merged.Resolve("p"); err != nil {
		t.Fatal(err)
	}
}
