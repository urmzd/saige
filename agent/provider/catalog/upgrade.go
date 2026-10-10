package catalog

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// WarnV1Catalog reports a catalog read from a version 1 file.
const WarnV1Catalog = "v1_catalog"

// Endpoint names UpgradeV1 creates for the built-in vendors.
const (
	EndpointAnthropic       = "anthropic"
	EndpointOpenAIChat      = "openai-chat"
	EndpointOpenAIResponses = "openai-responses"
	EndpointGoogleGemini    = "google-gemini"
	EndpointGoogleVertex    = "google-vertex"
	EndpointOllama          = "ollama"
)

// baselineTemplate is the offering template a v1 baseline becomes.
func baselineTemplate(vendor types.ProviderName) string { return "baseline." + string(vendor) }

// primaryEndpointV1 is the endpoint a version 1 row's knobs describe: the
// one its provider's adapter called.
func primaryEndpointV1(vendor types.ProviderName) string {
	switch vendor {
	case providerAnthropic:
		return EndpointAnthropic
	case providerOpenAI:
		return EndpointOpenAIChat
	case providerGoogle:
		return EndpointGoogleGemini
	case providerOllama:
		return EndpointOllama
	}
	return string(vendor)
}

// endpointsV1 are the endpoints a vendor gets: its primary endpoint, plus
// the other surfaces its adapter reaches, which inherit the primary's
// offerings.
//
//nolint:gosec // the auth secrets are references to environment variables, not credentials
func endpointsV1(vendor types.ProviderName, hasBaseline bool) map[string]EndpointSpec {
	def := ""
	if hasBaseline {
		def = baselineTemplate(vendor)
	}
	serves := []types.ProviderName{vendor}
	switch vendor {
	case providerAnthropic:
		return map[string]EndpointSpec{EndpointAnthropic: {Surface: types.SurfaceAnthropicMessages, Serves: serves, Primary: true,
			Auth: &AuthSpec{Type: authAPIKey, Secret: "env:ANTHROPIC_API_KEY"}, Files: &FilesSpec{API: true, URISchemes: []string{"https"}},
			Modes: &ModesSpec{Batch: true}, DefaultOfferingTemplate: def}}
	case providerOpenAI:
		return map[string]EndpointSpec{
			EndpointOpenAIChat: {Surface: types.SurfaceOpenAIChat, Serves: serves, Primary: true,
				Auth: &AuthSpec{Type: authAPIKey, Secret: "env:OPENAI_API_KEY"}, Files: &FilesSpec{API: true, URISchemes: []string{"https"}},
				Modes: &ModesSpec{Batch: true}, DefaultOfferingTemplate: def},
			EndpointOpenAIResponses: {Surface: types.SurfaceOpenAIResponses, Serves: serves, InheritOfferings: EndpointOpenAIChat,
				Auth: &AuthSpec{Type: authAPIKey, Secret: "env:OPENAI_API_KEY"}, Files: &FilesSpec{API: true, URISchemes: []string{"https"}},
				Modes: &ModesSpec{Batch: true}, DefaultOfferingTemplate: def},
		}
	case providerGoogle:
		return map[string]EndpointSpec{
			EndpointGoogleGemini: {Surface: types.SurfaceGeminiAPI, Serves: serves, Primary: true,
				Auth: &AuthSpec{Type: authAPIKey, Secret: "env:GOOGLE_API_KEY"}, Files: &FilesSpec{API: true, URISchemes: []string{"https"}},
				Modes: &ModesSpec{Batch: true}, DefaultOfferingTemplate: def},
			EndpointGoogleVertex: {Surface: types.SurfaceVertex, Serves: serves, InheritOfferings: EndpointGoogleGemini,
				Location: &LocationSpec{Region: "env:GOOGLE_CLOUD_LOCATION", Project: "env:GOOGLE_CLOUD_PROJECT"},
				Auth:     &AuthSpec{Type: "adc"}, Files: &FilesSpec{URISchemes: []string{"gs", "https"}},
				Modes: &ModesSpec{Batch: true}, DefaultOfferingTemplate: def},
		}
	case providerOllama:
		return map[string]EndpointSpec{EndpointOllama: {Surface: types.SurfaceOllamaNative, Serves: serves, Primary: true,
			Auth: &AuthSpec{Type: authNone}, Data: &DataSpec{ZeroRetention: true, PIIOK: true}, DefaultOfferingTemplate: def}}
	}
	return map[string]EndpointSpec{string(vendor): {Surface: string(vendor), Serves: serves, Primary: true, DefaultOfferingTemplate: def}}
}

// UpgradeV1 converts a version 1 catalog layer to version 2, field by
// field, so templates and overlay patches keep their meaning:
//
//   - each template becomes a model template (tier, limits, the modalities
//     of its media) and an offering template (features, parameters,
//     constraints, media, pricing, dials, notes) of the same name;
//   - each row becomes a model and an offering on its provider's primary
//     endpoint, and the other endpoints of the provider inherit it;
//   - each baseline becomes the offering template "baseline.<provider>",
//     the primary endpoint's default_offering_template;
//   - batch pricing moves to the batch service tier;
//   - presets are kept as they are; their entries are read in the legacy
//     form.
//
// A layer with baselines, or one that does not inherit the default, gets
// the endpoints of the providers it names. The returned issues report what
// could not be converted; Validate checks the result and warns that it was
// upgraded.
func UpgradeV1(c *CatalogV1) (*Catalog, []Issue) {
	var found issues
	out := &Catalog{Version: SchemaVersion, Schema: c.Schema, Revision: c.Revision, InheritDefault: c.InheritDefault,
		Presets: c.Presets, DefaultPreset: c.DefaultPreset, Dials: c.Dials,
		deletedPresets: slices.Clone(c.deletedPresets), upgraded: true}
	if len(c.Templates) > 0 {
		out.ModelTemplates, out.OfferingTemplates = map[string]ModelSpec{}, map[string]OfferingSpec{}
	}
	for name, t := range c.Templates {
		if isNullRaw(t.raw) {
			out.ModelTemplates[name] = ModelSpec{raw: []byte(jsonNull)}
			out.OfferingTemplates[name] = OfferingSpec{raw: []byte(jsonNull)}
			continue
		}
		out.ModelTemplates[name] = withNulls(modelFromV1(t), t.raw, modelNullKeys)
		out.OfferingTemplates[name] = withNulls(offeringFromV1(t), t.raw, offeringNullKeys)
	}
	standalone := c.InheritDefault != nil && !*c.InheritDefault
	vendors := map[types.ProviderName]bool{}
	for _, m := range c.Models {
		vendors[m.Provider] = true
	}
	for p, b := range c.Baselines {
		vendors[types.ProviderName(p)] = true
		if isNullRaw(b.raw) {
			continue
		}
		if out.OfferingTemplates == nil {
			out.OfferingTemplates = map[string]OfferingSpec{}
		}
		o := offeringFromV1(b)
		// A baseline has no model, so the limits its templates declare are
		// the offering's own.
		if l := c.baselineLimits(b); l != nil {
			o.Limits = l
		}
		out.OfferingTemplates[baselineTemplate(types.ProviderName(p))] = o
	}
	if len(c.Baselines) > 0 || standalone {
		out.Endpoints = map[string]EndpointSpec{}
		for _, v := range sortedKeys(vendors) {
			_, hasBaseline := c.Baselines[string(v)]
			for name, e := range endpointsV1(v, hasBaseline) {
				out.Endpoints[name] = e
			}
		}
	}
	if len(c.Models) > 0 {
		out.Models = map[string]ModelSpec{}
	}
	for i, m := range c.Models {
		k := modelKey(m.Provider, m.Prefix)
		if _, dup := out.Models[k]; dup {
			found.errorf(fmt.Sprintf("models[%d]", i), CodeDuplicateRow, "row %s is declared twice", k)
			continue
		}
		out.Models[k] = withNulls(modelFromV1(m), m.raw, modelNullKeys)
		o := offeringFromV1(m)
		o.Model, o.Endpoint = k, primaryEndpointV1(m.Provider)
		out.Offerings = append(out.Offerings, withNulls(o, m.raw, offeringNullKeys))
	}
	sortOfferings(out.Offerings)
	pruneModelTemplates(out)
	return out, found
}

// baselineLimits resolves the context and output limits a v1 baseline
// declares through its templates.
func (c *CatalogV1) baselineLimits(b ModelSpecV1) *ModelLimitsSpec {
	chain := []ModelSpecV1{b}
	seen := map[string]bool{}
	for next := b.Extends; next != "" && !seen[next] && len(chain) <= maxExtendsDepth; {
		seen[next] = true
		t, ok := c.Templates[next]
		if !ok {
			break
		}
		chain = append([]ModelSpecV1{t}, chain...)
		next = t.Extends
	}
	var out ModelLimitsSpec
	for _, s := range chain {
		if s.Limits == nil {
			continue
		}
		if s.Limits.ContextWindow != nil {
			out.ContextWindow = ptr(*s.Limits.ContextWindow)
		}
		if s.Limits.MaxOutputTokens != nil {
			out.MaxOutputTokens = ptr(*s.Limits.MaxOutputTokens)
		}
	}
	if out.ContextWindow == nil && out.MaxOutputTokens == nil {
		return nil
	}
	return &out
}

// pruneModelTemplates removes model templates that declare nothing,
// pointing what extended them at their parent.
func pruneModelTemplates(c *Catalog) {
	for {
		removed := false
		for name, t := range c.ModelTemplates {
			if isNullRaw(t.raw) {
				continue
			}
			bare := t
			bare.Extends, bare.raw = "", nil
			if !bare.isZero() {
				continue
			}
			parent := t.Extends
			relink := func(m ModelSpec) ModelSpec {
				if m.Extends == name {
					m.Extends, m.raw = parent, nil
				}
				return m
			}
			for k, m := range c.ModelTemplates {
				c.ModelTemplates[k] = relink(m)
			}
			for k, m := range c.Models {
				c.Models[k] = relink(m)
			}
			delete(c.ModelTemplates, name)
			removed = true
		}
		if !removed {
			return
		}
	}
}

func (m ModelSpec) isZero() bool {
	return m.Extends == "" && m.Tier == "" && m.SupersededBy == "" && m.Limits == nil && m.Modalities == nil &&
		m.Notes == nil && !m.Replace && !m.Delete
}

// modelFromV1 keeps the model facts of a v1 spec.
func modelFromV1(s ModelSpecV1) ModelSpec {
	m := ModelSpec{Extends: s.Extends, Tier: s.Tier, SupersededBy: s.SupersededBy, Replace: s.Replace, Delete: s.Delete}
	if l := s.Limits; l != nil && (l.ContextWindow != nil || l.MaxOutputTokens != nil) {
		m.Limits = &ModelLimitsSpec{ContextWindow: l.ContextWindow, MaxOutputTokens: l.MaxOutputTokens}
	}
	if s.Media != nil {
		m.Modalities = &ModelModalitiesSpec{In: modalitiesOf(s.Media)}
	}
	return m
}

// modalitiesOf lists text and the modalities of the media types, in the
// canonical order.
func modalitiesOf(media []types.MediaType) []types.Modality {
	out := []types.Modality{types.ModalityText}
	for _, m := range types.KnownModalities() {
		for _, mt := range media {
			if mt.Modality() == m && !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// offeringFromV1 keeps the request knobs and offering facts of a v1 spec.
//
//nolint:gocyclo // one flat mapping per v1 field; splitting it would scatter the conversion table
func offeringFromV1(s ModelSpecV1) OfferingSpec {
	o := OfferingSpec{Extends: s.Extends, Replace: s.Replace, Delete: s.Delete, StructuredOutput: s.StructuredOutput,
		ServerTools: s.ServerTools, ServerToolFees: s.ServerToolFees, Defaults: s.Defaults.clone(), Dials: s.Dials.clone(), Notes: s.Notes}
	param := func(p types.ParamName) *ParamSpec {
		if o.Params == nil {
			o.Params = map[types.ParamName]*ParamSpec{}
		}
		if o.Params[p] == nil {
			o.Params[p] = &ParamSpec{}
		}
		return o.Params[p]
	}
	allow := func(c types.Capability, v bool) bool {
		p, ok := types.CapabilityParam(c)
		if ok {
			param(p).Allowed = ptr(v)
		}
		return ok
	}
	if len(s.Capabilities) > 0 {
		for _, c := range s.Capabilities {
			if types.IsFeature(c) {
				o.Features = append(o.Features, c)
			}
		}
		if len(o.Features) == 0 {
			for _, c := range types.KnownCapabilities() {
				if types.IsFeature(c) {
					o.RemoveFeatures = append(o.RemoveFeatures, c)
				}
			}
		}
		for _, c := range types.KnownCapabilities() {
			if !types.IsFeature(c) {
				allow(c, slices.Contains(s.Capabilities, c))
			}
		}
	}
	for _, c := range s.AddCapabilities {
		if !allow(c, true) {
			o.AddFeatures = append(o.AddFeatures, c)
		}
	}
	for _, c := range s.RemoveCapabilities {
		if !allow(c, false) {
			o.RemoveFeatures = append(o.RemoveFeatures, c)
		}
	}
	if l := s.Limits; l != nil && l.DefaultMaxOutputTokens != nil {
		param(types.ParamMaxOutputTokens).Default = *l.DefaultMaxOutputTokens
	}
	if r := s.Reasoning; r != nil {
		if r.Efforts != nil {
			param(types.ParamReasoningEffort).Values = slices.Clone(r.Efforts)
		}
		if r.DefaultEffort != nil {
			param(types.ParamReasoningEffort).Default = *r.DefaultEffort
		}
		if r.Required != nil {
			for _, p := range []types.ParamName{types.ParamReasoningEnabled, types.ParamReasoningBudget, types.ParamReasoningEffort} {
				param(p).Required = ptr(*r.Required)
			}
		}
		if r.DefaultEnabled != nil {
			param(types.ParamReasoningEnabled).Default = *r.DefaultEnabled
		}
		if r.MinBudget != nil {
			param(types.ParamReasoningBudget).Min = ptr(float64(*r.MinBudget))
		}
		if r.MaxBudget != nil {
			param(types.ParamReasoningBudget).Max = ptr(float64(*r.MaxBudget))
		}
		special := func(key, meaning string, on *bool) {
			if on == nil {
				return
			}
			b := param(types.ParamReasoningBudget)
			if b.Special == nil {
				b.Special = map[string]string{}
			}
			b.Special[key] = ""
			if *on {
				b.Special[key] = meaning
			}
		}
		special("-1", types.SpecialDynamic, r.DynamicBudget)
		special("0", types.SpecialOff, r.ZeroBudget)
		if r.SamplingRequiresNoReasoning != nil {
			var forbid []types.ParamName
			for _, c := range r.SamplingRequiresNoReasoning {
				p, ok := types.CapabilityParam(c)
				if !ok {
					p = types.ParamName(c)
				}
				forbid = append(forbid, p)
			}
			if o.Constraints == nil {
				o.Constraints = map[string]*types.Constraint{}
			}
			for k, c := range types.SamplingConstraints(forbid) {
				if len(forbid) == 0 {
					o.Constraints[k] = nil
					continue
				}
				c := c
				o.Constraints[k] = &c
			}
		}
		if r.ForcedToolChoice != nil {
			tc := param(types.ParamToolChoice)
			tc.Values = types.StandardParam(types.ParamToolChoice).Values
			if !*r.ForcedToolChoice {
				tc.Values = []string{string(types.ToolChoiceNone)}
			}
		}
	}
	if s.ChatCompletionsTools != "" {
		if o.Constraints == nil {
			o.Constraints = map[string]*types.Constraint{}
		}
		o.Constraints[types.ConstraintChatTools] = nil
		if c, ok := types.ChatToolsConstraint(types.ChatCompletionsTools(s.ChatCompletionsTools)); ok {
			o.Constraints[types.ConstraintChatTools] = &c
		}
	}
	if s.Media != nil {
		o.Modalities = &ModalitiesSpec{In: map[types.Modality]*ModalityLimitSpec{}}
		for _, m := range types.KnownModalities() {
			if m == types.ModalityText {
				continue
			}
			var media []types.MediaType
			for _, mt := range s.Media {
				if mt.Modality() == m {
					media = append(media, mt)
				}
			}
			if media == nil {
				o.Modalities.In[m] = nil
				continue
			}
			o.Modalities.In[m] = &ModalityLimitSpec{Media: media}
		}
	}
	if p := s.Pricing; p != nil {
		o.Pricing = &PricingSpec{Currency: p.Currency, InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok,
			CachedInputPerMTok: p.CachedInputPerMTok, CacheWritePerMTok: p.CacheWritePerMTok, PerRequest: p.PerRequest,
			Free: p.Free, AsOf: p.AsOf, Source: p.Source}
		if p.BatchDiscount != 0 || p.BatchCachedInputPerMTok != 0 {
			o.Tiers = map[types.ServiceTier]*TierSpec{types.ServiceBatch: {Transport: types.TransportBatch, Discount: p.BatchDiscount,
				CachedInputPerMTok: p.BatchCachedInputPerMTok}}
		}
	}
	return o
}

// The v1 keys a null in an overlay row deletes, and the v2 keys that null
// becomes on the model and on the offering.
var (
	modelNullKeys = map[string][]string{"tier": {"tier"}, "superseded_by": {"superseded_by"}, "limits": {"limits"},
		"media": {"modalities"}, "extends": {"extends"}}
	offeringNullKeys = map[string][]string{"extends": {"extends"}, "capabilities": {"features"}, "media": {"modalities"},
		"structured_output": {"structured_output"}, "server_tools": {"server_tools"}, "server_tool_fees": {"server_tool_fees"},
		"pricing": {"pricing"}, "defaults": {"defaults"}, "dials": {"dials"}, "notes": {"notes"}}
)

// withNulls carries the top-level nulls of a v1 row into the raw form of
// what it became, so a merge still deletes what the null named.
func withNulls[T patchable[T]](v T, raw json.RawMessage, keys map[string][]string) T {
	if len(raw) == 0 || isNullRaw(raw) {
		return v
	}
	src, err := decodeObject(raw)
	if err != nil {
		return v
	}
	var nulls []string
	for k, val := range src {
		if val == nil {
			nulls = append(nulls, keys[k]...)
		}
	}
	if len(nulls) == 0 {
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	m, err := decodeObject(data)
	if err != nil {
		return v
	}
	for _, k := range nulls {
		m[k] = nil
	}
	data, err = json.Marshal(m)
	if err != nil {
		return v
	}
	return v.withRaw(data)
}
