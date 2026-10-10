package catalog

import (
	"fmt"
	"maps"
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// Export snapshots what Lookup resolves now, including rows added with
// Register and baselines set with RegisterBaseline, as a flat catalog:
// every model and offering written out in full, with no templates. The
// endpoints, presets, revision and default preset come from the active
// catalog. Loading the result and installing it reproduces the same
// lookups.
func Export() *Catalog {
	act := Active()
	mu.RLock()
	defer mu.RUnlock()
	out := &Catalog{Version: SchemaVersion, Revision: act.Revision, Presets: act.Presets, DefaultPreset: act.DefaultPreset,
		Endpoints: map[string]EndpointSpec{}}
	primary := map[types.ProviderName]string{}
	for name, ep := range act.Endpoints {
		ep = ep.cloneSpec()
		ep.DefaultOfferingTemplate = ""
		if ep.Overrides != nil {
			chain, _ := act.offeringChain("", *ep.Overrides, new(issues), true)
			flat := flatten(chain...)
			ep.Overrides = &flat
		}
		out.Endpoints[name] = ep
	}
	maps.Copy(primary, act.index(new(issues), true).primary)
	ensure := func(v types.ProviderName) string {
		if name, ok := primary[v]; ok {
			return name
		}
		for name, ep := range endpointsV1(v, false) {
			if _, have := out.Endpoints[name]; !have {
				out.Endpoints[name] = ep
			}
		}
		primary[v] = primaryEndpointV1(v)
		return primary[v]
	}
	for _, e := range globalView().entries {
		k := modelKey(e.Provider, e.Prefix)
		if out.Models == nil {
			out.Models = map[string]ModelSpec{}
		}
		caps := e.Caps.ForModel(string(e.Prefix))
		caps.Provider, caps.Family, caps.Known = string(e.Provider), string(e.Prefix), true
		offs := e.Offerings
		if offs == nil {
			offs = map[string]types.Offering{ensure(e.Provider): types.OfferingFromCapabilities(caps)}
		} else {
			ensure(e.Provider)
		}
		var info types.ModelInfo
		for _, name := range sortedKeys(offs) {
			info = offs[name].Model
			if name == primary[e.Provider] {
				break
			}
		}
		info.ContextWindow, info.MaxOutputTokens = caps.ContextWindow, caps.MaxOutputTokens
		ms := modelSpecOf(info)
		ms.Tier, ms.SupersededBy = e.Tier, e.SupersededBy
		out.Models[k] = ms
		for _, name := range sortedKeys(offs) {
			s := specFromOffering(offs[name])
			s.Model, s.Endpoint = k, name
			s.Defaults, s.Dials = e.Defaults.clone(), e.Dials.clone()
			s.ServerToolFees = maps.Clone(e.ServerToolFees)
			out.Offerings = append(out.Offerings, s)
		}
	}
	sortOfferings(out.Offerings)
	for _, p := range sortedKeys(baseline) {
		b := baseline[p]
		o := types.OfferingFromCapabilities(b)
		if b.Offering != nil {
			o = b.Offering.Clone()
		}
		s := specFromOffering(o)
		s.Limits = &ModelLimitsSpec{}
		if b.ContextWindow != 0 {
			s.Limits.ContextWindow = ptr(b.ContextWindow)
		}
		if b.MaxOutputTokens != 0 {
			s.Limits.MaxOutputTokens = ptr(b.MaxOutputTokens)
		}
		if s.Limits.ContextWindow == nil && s.Limits.MaxOutputTokens == nil {
			s.Limits = nil
		}
		if out.OfferingTemplates == nil {
			out.OfferingTemplates = map[string]OfferingSpec{}
		}
		name := baselineTemplate(p)
		out.OfferingTemplates[name] = s
		ep := out.Endpoints[ensure(p)]
		ep.DefaultOfferingTemplate = name
		out.Endpoints[primary[p]] = ep
	}
	// Endpoints that inherit another's offerings keep a baseline too.
	for name, ep := range out.Endpoints {
		for _, v := range ep.Serves {
			if _, ok := baseline[v]; ok && ep.DefaultOfferingTemplate == "" {
				ep.DefaultOfferingTemplate = baselineTemplate(v)
				out.Endpoints[name] = ep
			}
		}
	}
	return out
}

func (ep EndpointSpec) cloneSpec() EndpointSpec {
	out := ep
	out.Serves = slices.Clone(ep.Serves)
	out.ModelIDs = maps.Clone(ep.ModelIDs)
	if ep.Location != nil {
		out.Location = ptr(*ep.Location)
	}
	if ep.Auth != nil {
		out.Auth = ptr(*ep.Auth)
	}
	if ep.Transport != nil {
		out.Transport = ptr(*ep.Transport)
	}
	if ep.Capacity != nil {
		out.Capacity = ptr(*ep.Capacity)
	}
	if ep.Data != nil {
		d := *ep.Data
		if d.Store != nil {
			d.Store = ptr(*d.Store)
		}
		out.Data = &d
	}
	if ep.Files != nil {
		f := *ep.Files
		f.URISchemes = slices.Clone(f.URISchemes)
		out.Files = &f
	}
	if ep.Modes != nil {
		m := *ep.Modes
		if m.Streaming != nil {
			m.Streaming = ptr(*m.Streaming)
		}
		out.Modes = &m
	}
	if ep.Overrides != nil {
		o := flatten(*ep.Overrides)
		o.Extends = ep.Overrides.Extends
		out.Overrides = &o
	}
	out.raw = nil
	return out
}

// modelSpecOf writes resolved model facts as a self-contained model.
func modelSpecOf(m types.ModelInfo) ModelSpec {
	s := ModelSpec{Tier: Tier(m.Tier), SupersededBy: m.SupersededBy, Notes: slices.Clone(m.Notes)}
	l := ModelLimitsSpec{}
	if m.ContextWindow != 0 {
		l.ContextWindow = ptr(m.ContextWindow)
	}
	if m.MaxOutputTokens != 0 {
		l.MaxOutputTokens = ptr(m.MaxOutputTokens)
	}
	if l != (ModelLimitsSpec{}) {
		s.Limits = &l
	}
	if m.In != nil || m.Out != nil {
		s.Modalities = &ModelModalitiesSpec{In: slices.Clone(m.In), Out: slices.Clone(m.Out)}
	}
	return s
}

// specFromOffering writes a resolved offering as a self-contained spec.
func specFromOffering(o types.Offering) OfferingSpec {
	s := OfferingSpec{Features: slices.Clone(o.Features), ServerTools: slices.Clone(o.ServerTools),
		ServerToolFees: maps.Clone(o.ServerToolFees), Notes: slices.Clone(o.Notes)}
	if len(s.Features) == 0 {
		s.Features = nil
	}
	for name, p := range o.Params.Params {
		if s.Params == nil {
			s.Params = map[types.ParamName]*ParamSpec{}
		}
		fp := &ParamSpec{Type: p.Type, Min: p.Min, Max: p.Max, Values: slices.Clone(p.Values), Default: p.Default,
			Allowed: ptr(p.Accepted()), Special: maps.Clone(p.Special), Wire: p.Wire}
		if p.Required {
			fp.Required = ptr(true)
		}
		s.Params[name] = fp
	}
	for i, c := range o.Params.Constraints {
		if s.Constraints == nil {
			s.Constraints = map[string]*types.Constraint{}
		}
		c := c.Clone()
		s.Constraints[constraintName(i, c)] = &c
	}
	if len(o.Modalities.In) > 0 || len(o.Modalities.Out) > 0 || len(o.Modalities.ToolResult) > 0 {
		s.Modalities = &ModalitiesSpec{In: limitSpecs(o.Modalities.In), Out: limitSpecs(o.Modalities.Out),
			ToolResult: maps.Clone(o.Modalities.ToolResult)}
	}
	if o.StructuredOutput != types.StructuredOutputNone {
		s.StructuredOutput = ptr(o.StructuredOutput)
	}
	s.Pricing = pricingSpec(o.Pricing)
	for t, ts := range o.Tiers {
		if s.Tiers == nil {
			s.Tiers = map[types.ServiceTier]*TierSpec{}
		}
		spec := &TierSpec{Transport: ts.Transport, Discount: ts.Discount, CachedInputPerMTok: ts.CachedInputPerMTok, Wire: ts.Wire}
		if ts.Pricing != nil {
			spec.Pricing = pricingSpec(*ts.Pricing)
		}
		s.Tiers[t] = spec
	}
	for m, r := range o.ModalityPricing {
		if s.ModalityPricing == nil {
			s.ModalityPricing = map[types.Modality]*types.ModalityRate{}
		}
		s.ModalityPricing[m] = ptr(r)
	}
	if f := o.Fallback; len(f.Equivalents) > 0 || len(f.LargerContext) > 0 {
		s.Fallback = &FallbackSpec{Equivalents: slices.Clone(f.Equivalents), LargerContext: slices.Clone(f.LargerContext)}
	}
	return s
}

// constraintName names a constraint by the rule it states, so a written
// catalog keeps the names templates override.
func constraintName(i int, c types.Constraint) string {
	if len(c.Exclusive) > 0 && len(c.When) == 0 && len(c.Forbid) == 0 && len(c.Require) == 0 {
		return types.ConstraintReasoningExclusive
	}
	if _, ok := c.When[types.CondRequestSurface]; ok {
		return types.ConstraintChatTools
	}
	if len(c.When) == 1 && len(c.Forbid) > 0 {
		for k := range c.When {
			return "sampling." + k
		}
	}
	return fmt.Sprintf("constraint.%d", i)
}

func limitSpecs(in map[types.Modality]types.ModalityLimit) map[types.Modality]*ModalityLimitSpec {
	var out map[types.Modality]*ModalityLimitSpec
	for m, l := range in {
		if out == nil {
			out = map[types.Modality]*ModalityLimitSpec{}
		}
		s := &ModalityLimitSpec{Media: slices.Clone(l.Media), Sources: slices.Clone(l.Sources)}
		if l.MaxBytes != 0 {
			s.MaxBytes = ptr(l.MaxBytes)
		}
		if l.MaxCount != 0 {
			s.MaxCount = ptr(l.MaxCount)
		}
		if l.MaxPixels != 0 {
			s.MaxPixels = ptr(l.MaxPixels)
		}
		if l.MaxPages != 0 {
			s.MaxPages = ptr(l.MaxPages)
		}
		if l.MaxDuration != 0 {
			s.MaxDuration = ptr(Duration(l.MaxDuration))
		}
		if l.FPS != nil {
			s.FPS = &ParamSpec{Type: l.FPS.Type, Min: l.FPS.Min, Max: l.FPS.Max, Default: l.FPS.Default}
		}
		if l.Tokens != (types.TokenRule{}) {
			s.Tokens = ptr(l.Tokens)
		}
		out[m] = s
	}
	return out
}

func ptr[T any](v T) *T { return &v }
