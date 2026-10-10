package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// maxExtendsDepth bounds a template chain.
const maxExtendsDepth = 4

// modelKey is the identity of a model: "<vendor>/<prefix>".
func modelKey(vendor types.ProviderName, prefix types.ModelID) string {
	return string(vendor) + "/" + string(prefix)
}

// splitModelKey splits a model key at its first slash.
func splitModelKey(k string) (types.ProviderName, types.ModelID, bool) {
	v, p, ok := strings.Cut(k, "/")
	if !ok || v == "" || strings.TrimSpace(p) == "" {
		return "", "", false
	}
	return types.ProviderName(v), types.ModelID(p), true
}

// OfferingID is the identity of an offering:
// "<vendor>/<prefix>@<endpoint>".
func OfferingID(model, endpoint string) string { return model + "@" + endpoint }

// ParseOfferingID splits an offering ID into its model key and endpoint.
func ParseOfferingID(id string) (model, endpoint string, ok bool) {
	i := strings.LastIndex(id, "@")
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	if _, _, ok := splitModelKey(id[:i]); !ok {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

func sortOfferings(o []OfferingSpec) {
	sort.SliceStable(o, func(i, j int) bool {
		return OfferingID(o[i].Model, o[i].Endpoint) < OfferingID(o[j].Model, o[j].Endpoint)
	})
}

// isNullRaw reports whether a raw value is the JSON null a merge patch
// deletes with.
func isNullRaw(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == jsonNull }

// ── Models ──────────────────────────────────────────────────────────

// modelChain returns the templates s extends, root first. Missing templates
// are reported only when full is set, since a layer may extend a template a
// lower layer defines.
func (c *Catalog) modelChain(path string, s ModelSpec, found *issues, full bool) ([]ModelSpec, bool) {
	var chain []ModelSpec
	seen := map[string]bool{}
	for next, depth := s.Extends, 0; next != ""; depth++ {
		if seen[next] {
			found.errorf(path+".extends", CodeExtendsCycle, "template chain loops at %q", next)
			return nil, false
		}
		if depth >= maxExtendsDepth {
			found.errorf(path+".extends", CodeExtendsDepth, "template chain is deeper than %d", maxExtendsDepth)
			return nil, false
		}
		seen[next] = true
		t, ok := c.ModelTemplates[next]
		if !ok {
			if full {
				found.errorf(path+".extends", CodeUnknownTemplate, "unknown model template %q", next)
			}
			return nil, false
		}
		chain = append([]ModelSpec{t}, chain...)
		next = t.Extends
	}
	return chain, true
}

// resolvedModel is a model with its templates applied.
type resolvedModel struct {
	info types.ModelInfo
	tier Tier
	// inDeclared is set once a modality list is declared, so an offering's
	// modalities are narrowed to it.
	inDeclared bool
}

// resolveModel resolves one model through its templates.
func (c *Catalog) resolveModel(path, key string, s ModelSpec, found *issues, full bool) (resolvedModel, bool) {
	chain, ok := c.modelChain(path, s, found, full)
	if !ok {
		return resolvedModel{}, false
	}
	vendor, prefix, _ := splitModelKey(key)
	m := resolvedModel{info: types.ModelInfo{Vendor: vendor, Prefix: prefix, Known: true}}
	for _, t := range append(chain, s) {
		t.applyTo(&m)
	}
	return m, true
}

func (s ModelSpec) applyTo(m *resolvedModel) {
	if s.Tier != "" {
		m.tier, m.info.Tier = s.Tier, string(s.Tier)
	}
	if s.SupersededBy != "" {
		m.info.SupersededBy = s.SupersededBy
	}
	if l := s.Limits; l != nil {
		setInt(&m.info.ContextWindow, l.ContextWindow)
		setInt(&m.info.MaxOutputTokens, l.MaxOutputTokens)
	}
	if md := s.Modalities; md != nil {
		if md.In != nil {
			m.info.In, m.inDeclared = slices.Clone(md.In), true
		}
		if md.Out != nil {
			m.info.Out = slices.Clone(md.Out)
		}
	}
	if s.Notes != nil {
		m.info.Notes = slices.Clone(s.Notes)
	}
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}

// ── Offerings ───────────────────────────────────────────────────────

// offeringChain returns s with the templates it extends, root first.
func (c *Catalog) offeringChain(path string, s OfferingSpec, found *issues, full bool) ([]OfferingSpec, bool) {
	chain := []OfferingSpec{s}
	seen := map[string]bool{}
	for next, depth := s.Extends, 0; next != ""; depth++ {
		if seen[next] {
			found.errorf(path+".extends", CodeExtendsCycle, "template chain loops at %q", next)
			return nil, false
		}
		if depth >= maxExtendsDepth {
			found.errorf(path+".extends", CodeExtendsDepth, "template chain is deeper than %d", maxExtendsDepth)
			return nil, false
		}
		seen[next] = true
		t, ok := c.OfferingTemplates[next]
		if !ok {
			if full {
				found.errorf(path+".extends", CodeUnknownTemplate, "unknown offering template %q", next)
			}
			return nil, false
		}
		chain = append([]OfferingSpec{t}, chain...)
		next = t.Extends
	}
	return chain, true
}

// flatten applies specs in order onto an empty offering.
func flatten(specs ...OfferingSpec) OfferingSpec {
	var acc OfferingSpec
	for _, s := range specs {
		s.applyTo(&acc)
	}
	return acc
}

// applyTo layers s onto a partially resolved offering. Set fields replace;
// features replace, then add, then remove; parameters, constraints,
// modalities and tiers merge per key.
//
//nolint:gocyclo // one merge rule per field; splitting it would scatter the rules
func (s OfferingSpec) applyTo(acc *OfferingSpec) {
	if len(s.Features) > 0 {
		acc.Features = slices.Clone(s.Features)
	}
	for _, f := range s.AddFeatures {
		if !slices.Contains(acc.Features, f) {
			acc.Features = append(acc.Features, f)
		}
	}
	for _, f := range s.RemoveFeatures {
		acc.Features = slices.DeleteFunc(acc.Features, func(x types.Capability) bool { return x == f })
	}
	for name, p := range s.Params {
		if p == nil {
			delete(acc.Params, name)
			continue
		}
		if acc.Params == nil {
			acc.Params = map[types.ParamName]*ParamSpec{}
		}
		cur := ParamSpec{}
		if acc.Params[name] != nil {
			cur = acc.Params[name].clone()
		}
		cur.merge(*p)
		acc.Params[name] = &cur
	}
	for k, cn := range s.Constraints {
		if cn == nil {
			delete(acc.Constraints, k)
			continue
		}
		if acc.Constraints == nil {
			acc.Constraints = map[string]*types.Constraint{}
		}
		v := cn.Clone()
		acc.Constraints[k] = &v
	}
	if m := s.Modalities; m != nil {
		if acc.Modalities == nil {
			acc.Modalities = &ModalitiesSpec{}
		}
		acc.Modalities.In = mergeLimitSpecs(acc.Modalities.In, m.In)
		acc.Modalities.Out = mergeLimitSpecs(acc.Modalities.Out, m.Out)
		for k, v := range m.ToolResult {
			if v == "" {
				delete(acc.Modalities.ToolResult, k)
				continue
			}
			if acc.Modalities.ToolResult == nil {
				acc.Modalities.ToolResult = map[types.Modality]string{}
			}
			acc.Modalities.ToolResult[k] = v
		}
	}
	if l := s.Limits; l != nil {
		if acc.Limits == nil {
			acc.Limits = &ModelLimitsSpec{}
		}
		if l.ContextWindow != nil {
			acc.Limits.ContextWindow = ptr(*l.ContextWindow)
		}
		if l.MaxOutputTokens != nil {
			acc.Limits.MaxOutputTokens = ptr(*l.MaxOutputTokens)
		}
	}
	if s.StructuredOutput != nil {
		acc.StructuredOutput = ptr(*s.StructuredOutput)
	}
	if s.ServerTools != nil {
		acc.ServerTools = slices.Clone(s.ServerTools)
	}
	if s.ServerToolFees != nil {
		acc.ServerToolFees = maps.Clone(s.ServerToolFees)
	}
	if s.Pricing != nil {
		acc.Pricing = ptr(*s.Pricing)
	}
	for t, spec := range s.Tiers {
		if spec == nil {
			delete(acc.Tiers, t)
			continue
		}
		if acc.Tiers == nil {
			acc.Tiers = map[types.ServiceTier]*TierSpec{}
		}
		acc.Tiers[t] = spec.clone()
	}
	for m, r := range s.ModalityPricing {
		if r == nil {
			delete(acc.ModalityPricing, m)
			continue
		}
		if acc.ModalityPricing == nil {
			acc.ModalityPricing = map[types.Modality]*types.ModalityRate{}
		}
		acc.ModalityPricing[m] = ptr(*r)
	}
	if s.Defaults != nil {
		acc.Defaults = s.Defaults.clone()
	}
	if s.Dials != nil {
		acc.Dials = acc.Dials.merge(s.Dials)
	}
	if s.Fallback != nil {
		acc.Fallback = &FallbackSpec{Equivalents: slices.Clone(s.Fallback.Equivalents), LargerContext: slices.Clone(s.Fallback.LargerContext)}
	}
	if s.Notes != nil {
		acc.Notes = slices.Clone(s.Notes)
	}
}

func (t *TierSpec) clone() *TierSpec {
	out := *t
	if t.Pricing != nil {
		out.Pricing = ptr(*t.Pricing)
	}
	return &out
}

func mergeLimitSpecs(base, over map[types.Modality]*ModalityLimitSpec) map[types.Modality]*ModalityLimitSpec {
	for m, l := range over {
		if l == nil {
			delete(base, m)
			continue
		}
		if base == nil {
			base = map[types.Modality]*ModalityLimitSpec{}
		}
		cur := ModalityLimitSpec{}
		if b := base[m]; b != nil {
			cur = *b
		}
		if l.Media != nil {
			cur.Media = slices.Clone(l.Media)
		}
		if l.Sources != nil {
			cur.Sources = slices.Clone(l.Sources)
		}
		if l.MaxCount != nil {
			cur.MaxCount = ptr(*l.MaxCount)
		}
		if l.MaxPixels != nil {
			cur.MaxPixels = ptr(*l.MaxPixels)
		}
		if l.MaxPages != nil {
			cur.MaxPages = ptr(*l.MaxPages)
		}
		if l.MaxBytes != nil {
			cur.MaxBytes = ptr(*l.MaxBytes)
		}
		if l.MaxDuration != nil {
			cur.MaxDuration = ptr(*l.MaxDuration)
		}
		if l.FPS != nil {
			f := l.FPS.clone()
			cur.FPS = &f
		}
		if l.Tokens != nil {
			cur.Tokens = ptr(*l.Tokens)
		}
		base[m] = &cur
	}
	return base
}

func (p ParamSpec) clone() ParamSpec {
	out := p
	if p.Min != nil {
		out.Min = ptr(*p.Min)
	}
	if p.Max != nil {
		out.Max = ptr(*p.Max)
	}
	if p.Allowed != nil {
		out.Allowed = ptr(*p.Allowed)
	}
	if p.Required != nil {
		out.Required = ptr(*p.Required)
	}
	out.Values = slices.Clone(p.Values)
	out.Special = maps.Clone(p.Special)
	return out
}

// merge layers the fields over sets onto p. An empty special meaning
// removes that special value.
func (p *ParamSpec) merge(over ParamSpec) {
	if over.Type != "" {
		p.Type = over.Type
	}
	if over.Min != nil {
		p.Min = ptr(*over.Min)
	}
	if over.Max != nil {
		p.Max = ptr(*over.Max)
	}
	if over.Values != nil {
		p.Values = slices.Clone(over.Values)
	}
	if over.Default != nil {
		p.Default = over.Default
	}
	if over.Allowed != nil {
		p.Allowed = ptr(*over.Allowed)
	}
	if over.Required != nil {
		p.Required = ptr(*over.Required)
	}
	for k, v := range over.Special {
		if v == "" {
			delete(p.Special, k)
			continue
		}
		if p.Special == nil {
			p.Special = map[string]string{}
		}
		p.Special[k] = v
	}
	if over.Wire != "" {
		p.Wire = over.Wire
	}
}

// accepted reports whether a resolved parameter may be sent: allowed true,
// or a declared type with allowed unset.
func (p ParamSpec) accepted() bool {
	if p.Allowed != nil {
		return *p.Allowed
	}
	return p.Type != ""
}

// paramSpec converts a resolved parameter to the read model, filling what
// it leaves unset from the standard specification.
func (p ParamSpec) paramSpec(name types.ParamName) types.ParamSpec {
	std := types.StandardParam(name)
	out := types.ParamSpec{Type: cmpOr(p.Type, std.Type), Min: p.Min, Max: p.Max, Values: slices.Clone(p.Values),
		Default: normalizeDefault(p.Default), Wire: p.Wire, Allowed: ptr(p.accepted())}
	if out.Min == nil && std.Min != nil {
		out.Min = ptr(*std.Min)
	}
	if out.Max == nil && std.Max != nil {
		out.Max = ptr(*std.Max)
	}
	if out.Values == nil {
		out.Values = slices.Clone(std.Values)
	}
	if p.Required != nil {
		out.Required = *p.Required
	}
	for k, v := range p.Special {
		if v == "" {
			continue
		}
		if out.Special == nil {
			out.Special = map[string]string{}
		}
		out.Special[k] = v
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// normalizeDefault turns a decoded JSON number into an int when it is
// integral and a float64 otherwise.
func normalizeDefault(v any) any {
	var f float64
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		f, _ = x.Float64()
		return f
	case float64:
		f = x
	case int64:
		return int(x)
	default:
		return v
	}
	if f == float64(int(f)) {
		return int(f)
	}
	return f
}

// buildOffering resolves an offering's read model from its flattened spec.
func buildOffering(id string, m resolvedModel, epName string, ep EndpointSpec, flat OfferingSpec) types.Offering {
	o := types.Offering{ID: id, Model: m.info, Endpoint: endpointInfo(epName, ep, m.info.Prefix)}
	o.Model.In, o.Model.Out, o.Model.Notes = slices.Clone(m.info.In), slices.Clone(m.info.Out), slices.Clone(m.info.Notes)
	for _, f := range flat.Features {
		if !slices.Contains(o.Features, f) {
			o.Features = append(o.Features, f)
		}
	}
	slices.Sort(o.Features)
	o.Params.Params = map[types.ParamName]types.ParamSpec{}
	for name, p := range flat.Params {
		o.Params.Params[name] = p.paramSpec(name)
	}
	keys := slices.Sorted(maps.Keys(flat.Constraints))
	if _, ok := flat.Constraints[types.ConstraintReasoningExclusive]; !ok {
		keys = append(keys, types.ConstraintReasoningExclusive)
		slices.Sort(keys)
	}
	for _, k := range keys {
		if cn, ok := flat.Constraints[k]; ok {
			o.Params.Constraints = append(o.Params.Constraints, cn.Clone())
		} else {
			o.Params.Constraints = append(o.Params.Constraints, types.ReasoningExclusive())
		}
	}
	if md := flat.Modalities; md != nil {
		o.Modalities.In = limitsOf(md.In, m)
		o.Modalities.Out = limitsOf(md.Out, resolvedModel{})
		o.Modalities.ToolResult = maps.Clone(md.ToolResult)
	}
	if l := flat.Limits; l != nil {
		if l.ContextWindow != nil {
			o.Model.ContextWindow = minNonZero(o.Model.ContextWindow, *l.ContextWindow)
		}
		if l.MaxOutputTokens != nil {
			o.Model.MaxOutputTokens = minNonZero(o.Model.MaxOutputTokens, *l.MaxOutputTokens)
		}
	}
	if flat.StructuredOutput != nil {
		o.StructuredOutput = *flat.StructuredOutput
	}
	// A mode without the capability is not a declaration.
	if !slices.Contains(o.Features, types.CapStructuredOutput) {
		o.StructuredOutput = types.StructuredOutputNone
	}
	o.ServerTools = slices.Clone(flat.ServerTools)
	o.ServerToolFees = maps.Clone(flat.ServerToolFees)
	if flat.Pricing != nil {
		o.Pricing = flat.Pricing.pricing()
	}
	for t, spec := range flat.Tiers {
		if spec.Transport == types.TransportBatch && (ep.Modes == nil || !ep.Modes.Batch) {
			continue // the endpoint has no batch mode to serve it
		}
		if o.Tiers == nil {
			o.Tiers = map[types.ServiceTier]types.TierSpec{}
		}
		ts := types.TierSpec{Transport: spec.Transport, Discount: spec.Discount, CachedInputPerMTok: spec.CachedInputPerMTok, Wire: spec.Wire}
		if spec.Pricing != nil {
			ts.Pricing = ptr(spec.Pricing.pricing())
		}
		o.Tiers[t] = ts
	}
	for mo, r := range flat.ModalityPricing {
		if o.ModalityPricing == nil {
			o.ModalityPricing = map[types.Modality]types.ModalityRate{}
		}
		o.ModalityPricing[mo] = *r
	}
	o.Defaults = flat.Defaults.requestOptions()
	o.DialMap = flat.Dials.dialMap()
	if f := flat.Fallback; f != nil {
		o.Fallback = types.FallbackHints{Equivalents: slices.Clone(f.Equivalents), LargerContext: slices.Clone(f.LargerContext)}
	}
	o.Notes = slices.Clone(flat.Notes)
	return o
}

func minNonZero(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// limitsOf converts declared modality limits, narrowed to the modalities
// the model declares it takes, when it declares them.
func limitsOf(in map[types.Modality]*ModalityLimitSpec, m resolvedModel) map[types.Modality]types.ModalityLimit {
	var out map[types.Modality]types.ModalityLimit
	for mo, l := range in {
		if l == nil || (m.inDeclared && mo != types.ModalityText && !slices.Contains(m.info.In, mo)) {
			continue
		}
		lim := types.ModalityLimit{Media: slices.Clone(l.Media), Sources: slices.Clone(l.Sources)}
		if l.MaxBytes != nil {
			lim.MaxBytes = *l.MaxBytes
		}
		if l.MaxCount != nil {
			lim.MaxCount = *l.MaxCount
		}
		if l.MaxPixels != nil {
			lim.MaxPixels = *l.MaxPixels
		}
		if l.MaxPages != nil {
			lim.MaxPages = *l.MaxPages
		}
		if l.MaxDuration != nil {
			lim.MaxDuration = time.Duration(*l.MaxDuration)
		}
		if l.FPS != nil {
			f := l.FPS.paramSpec("fps")
			lim.FPS = &f
		}
		if l.Tokens != nil {
			lim.Tokens = *l.Tokens
		}
		if out == nil {
			out = map[types.Modality]types.ModalityLimit{}
		}
		out[mo] = lim
	}
	return out
}

// endpointInfo is the part of an endpoint an offering carries.
func endpointInfo(name string, ep EndpointSpec, prefix types.ModelID) types.EndpointInfo {
	info := types.EndpointInfo{Name: name, Surface: ep.Surface, ModelID: ep.ModelIDs[prefix], Streaming: true}
	if d := ep.Data; d != nil {
		info.Data = types.DataHandling{ZeroRetention: d.ZeroRetention, Residency: d.Residency, PIIOK: d.PIIOK}
		if d.Store != nil {
			info.Data.Store = ptr(*d.Store)
		}
	}
	if f := ep.Files; f != nil {
		info.Files = types.FileSupport{API: f.API, MaxBytes: f.MaxBytes, TTL: time.Duration(f.TTL), URISchemes: slices.Clone(f.URISchemes)}
	}
	if md := ep.Modes; md != nil {
		info.Batch = md.Batch
		if md.Streaming != nil {
			info.Streaming = *md.Streaming
		}
	}
	return info
}

// ── Index ───────────────────────────────────────────────────────────

// resolvedOffering is one offering with the spec it was built from.
type resolvedOffering struct {
	off  types.Offering
	flat OfferingSpec
	// explicit is set for an offering the catalog lists, rather than one
	// inherited from another endpoint or made from a default template.
	explicit bool
}

// index is a catalog with every model, endpoint and offering resolved.
type index struct {
	models    map[string]resolvedModel
	endpoints map[string]EndpointSpec
	// offerings maps an endpoint to its offerings by model key.
	offerings map[string]map[string]*resolvedOffering
	// baselines maps an endpoint and vendor to the offering an unlisted
	// model gets there.
	baselines map[string]map[types.ProviderName]*resolvedOffering
	primary   map[types.ProviderName]string
}

// index resolves the catalog. Problems are reported to found; what fails
// to resolve is left out.
func (c *Catalog) index(found *issues, full bool) *index {
	ix := &index{models: map[string]resolvedModel{}, endpoints: c.Endpoints,
		offerings: map[string]map[string]*resolvedOffering{}, baselines: map[string]map[types.ProviderName]*resolvedOffering{},
		primary: map[types.ProviderName]string{}}
	for _, k := range sortedKeys(c.Models) {
		m := c.Models[k]
		if m.Delete {
			continue
		}
		if _, _, ok := splitModelKey(k); !ok {
			continue
		}
		if rm, ok := c.resolveModel("models."+k, k, m, found, full); ok {
			ix.models[k] = rm
		}
	}
	for _, name := range sortedKeys(c.Endpoints) {
		ep := c.Endpoints[name]
		for _, v := range ep.Serves {
			if _, dup := ix.primary[v]; ep.Primary && !dup {
				ix.primary[v] = name
			}
		}
	}
	for _, name := range sortedKeys(c.Endpoints) {
		for _, v := range c.Endpoints[name].Serves {
			if _, ok := ix.primary[v]; !ok {
				ix.primary[v] = name // the first endpoint serving a vendor without a primary
			}
		}
	}
	explicit := map[string][]int{}
	for i, o := range c.Offerings {
		if !o.Delete {
			explicit[o.Endpoint] = append(explicit[o.Endpoint], i)
		}
	}
	done, visiting := map[string]bool{}, map[string]bool{}
	var build func(name string)
	build = func(name string) {
		if done[name] || visiting[name] {
			return
		}
		visiting[name] = true
		ep := c.Endpoints[name]
		if ep.InheritOfferings != "" {
			build(ep.InheritOfferings)
		}
		c.buildEndpoint(ix, name, ep, explicit[name], found, full)
		visiting[name], done[name] = false, true
	}
	for _, name := range sortedKeys(c.Endpoints) {
		build(name)
	}
	return ix
}

// overrides returns an endpoint's override chain.
func (c *Catalog) overrides(name string, ep EndpointSpec, found *issues, full bool) []OfferingSpec {
	if ep.Overrides == nil {
		return nil
	}
	chain, _ := c.offeringChain("endpoints."+name+".overrides", *ep.Overrides, found, full)
	return chain
}

func (c *Catalog) buildEndpoint(ix *index, name string, ep EndpointSpec, rows []int, found *issues, full bool) {
	offs := map[string]*resolvedOffering{}
	ix.offerings[name] = offs
	over := c.overrides(name, ep, found, full)
	for _, i := range rows {
		row := c.Offerings[i]
		m, ok := ix.models[row.Model]
		if !ok {
			continue
		}
		chain, ok := c.offeringChain(fmt.Sprintf("offerings[%d]", i), row, found, full)
		if !ok {
			continue
		}
		specs := append(slices.Clone(chain[:len(chain)-1]), over...)
		flat := flatten(append(specs, chain[len(chain)-1])...)
		offs[row.Model] = &resolvedOffering{off: buildOffering(OfferingID(row.Model, name), m, name, ep, flat), flat: flat, explicit: true}
	}
	if src, ok := ix.offerings[ep.InheritOfferings]; ok && ep.InheritOfferings != name {
		for _, k := range slices.Sorted(maps.Keys(src)) {
			m := ix.models[k]
			if _, have := offs[k]; have || !slices.Contains(ep.Serves, m.info.Vendor) {
				continue
			}
			flat := flatten(append([]OfferingSpec{src[k].flat}, over...)...)
			offs[k] = &resolvedOffering{off: buildOffering(OfferingID(k, name), m, name, ep, flat), flat: flat}
		}
	}
	if ep.DefaultOfferingTemplate == "" {
		return
	}
	tmpl, ok := c.offeringChain("endpoints."+name+".default_offering_template",
		OfferingSpec{Extends: ep.DefaultOfferingTemplate}, found, full)
	if !ok {
		return
	}
	base := flatten(append(tmpl, over...)...)
	for _, k := range slices.Sorted(maps.Keys(ix.models)) {
		m := ix.models[k]
		if _, have := offs[k]; have || !slices.Contains(ep.Serves, m.info.Vendor) {
			continue
		}
		flat := flatten(base)
		offs[k] = &resolvedOffering{off: buildOffering(OfferingID(k, name), m, name, ep, flat), flat: flat}
	}
	ix.baselines[name] = map[types.ProviderName]*resolvedOffering{}
	for _, v := range ep.Serves {
		m := resolvedModel{info: types.ModelInfo{Vendor: v}}
		flat := flatten(base)
		ix.baselines[name][v] = &resolvedOffering{off: buildOffering(string(v)+"/*@"+name, m, name, ep, flat), flat: flat}
	}
}

// entry builds the row Lookup serves for a model: its offering on the
// vendor's primary endpoint, or else the first endpoint that offers it.
func (ix *index) entry(key string) (Entry, bool) {
	m, ok := ix.models[key]
	if !ok {
		return Entry{}, false
	}
	var ro *resolvedOffering
	if p, ok := ix.primary[m.info.Vendor]; ok {
		ro = ix.offerings[p][key]
	}
	if ro == nil {
		for _, name := range sortedKeys(ix.offerings) {
			if r := ix.offerings[name][key]; r != nil {
				ro = r
				break
			}
		}
	}
	if ro == nil {
		return Entry{}, false
	}
	e := Entry{Provider: m.info.Vendor, Prefix: m.info.Prefix, Tier: m.tier, SupersededBy: m.info.SupersededBy,
		Caps: ro.off.Capabilities(), ServerToolFees: maps.Clone(ro.flat.ServerToolFees),
		Defaults: ro.flat.Defaults.clone(), Dials: ro.flat.Dials.clone(), Offerings: map[string]types.Offering{}}
	e.Caps.Provider, e.Caps.Model, e.Caps.Family, e.Caps.Known = "", "", "", false
	for name, offs := range ix.offerings {
		if r := offs[key]; r != nil {
			e.Offerings[name] = r.off.Clone()
		}
	}
	return e, true
}

// baseline returns the capabilities an unlisted model of a vendor gets.
func (ix *index) baseline(vendor types.ProviderName) (types.ModelCapabilities, bool) {
	p, ok := ix.primary[vendor]
	if !ok {
		return types.ModelCapabilities{}, false
	}
	b := ix.baselines[p][vendor]
	if b == nil {
		return types.ModelCapabilities{}, false
	}
	caps := b.off.Capabilities()
	caps.Provider, caps.Model, caps.Family = "", "", ""
	return caps, true
}

// view resolves the catalog's models and baselines without installing
// them. What fails to resolve is left out; check reports it.
func (c *Catalog) view() view {
	var found issues
	return indexView(c.index(&found, true))
}

func indexView(ix *index) view {
	v := view{baselines: map[types.ProviderName]types.ModelCapabilities{}, index: ix}
	for _, k := range slices.Sorted(maps.Keys(ix.models)) {
		if e, ok := ix.entry(k); ok {
			v.entries = append(v.entries, e)
		}
	}
	for vendor := range ix.primary {
		if b, ok := ix.baseline(vendor); ok {
			v.baselines[vendor] = b
		}
	}
	return v
}

// Lookup resolves a model against this catalog value, with the same rules as
// the package-level Lookup, without installing anything.
func (c *Catalog) Lookup(provider types.ProviderName, model types.ModelID) (types.ModelCapabilities, bool) {
	return c.view().lookup(provider, string(model))
}

// Describe returns the row of this catalog value that serves a model.
func (c *Catalog) Describe(provider types.ProviderName, model types.ModelID) (Entry, bool) {
	e, ok := c.view().match(provider, string(model))
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(e), true
}

// Offering returns the offering that serves a model on an endpoint: the
// offering of the model family that matches, otherwise the endpoint's
// baseline with Known false. The bool is false when the endpoint offers
// nothing for the vendor.
func (c *Catalog) Offering(endpoint string, provider types.ProviderName, model types.ModelID) (types.Offering, bool) {
	return c.view().offering(endpoint, provider, string(model))
}

// PrimaryEndpoint names the endpoint Lookup resolves a vendor's models on.
func (c *Catalog) PrimaryEndpoint(vendor types.ProviderName) (string, bool) {
	var found issues
	p, ok := c.index(&found, true).primary[vendor]
	return p, ok
}

// ResolvedOfferings lists every offering the catalog resolves, explicit,
// inherited or made from a default template, sorted by endpoint and model.
func (c *Catalog) ResolvedOfferings() []types.Offering {
	var found issues
	ix := c.index(&found, true)
	var out []types.Offering
	for _, name := range sortedKeys(ix.offerings) {
		for _, k := range sortedKeys(ix.offerings[name]) {
			out = append(out, ix.offerings[name][k].off.Clone())
		}
	}
	return out
}
