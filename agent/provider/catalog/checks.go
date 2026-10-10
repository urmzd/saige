package catalog

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Endpoint auth types.
const (
	authAPIKey = "api_key"
	authADC    = "adc"
	authNone   = "none"
)

// Issue codes for the version 2 objects.
const (
	CodeSurface     = "invalid_surface"
	CodeSecretRef   = "invalid_secret_ref"
	CodeParam       = "invalid_param"
	CodeModality    = "invalid_modality"
	CodeTier        = "invalid_tier"
	CodeDuplicateOf = "duplicate_offering"
	CodePrimary     = "duplicate_primary"
)

// Validate checks the catalog as a complete, merged value: every model,
// template, endpoint and offering resolves, references and successors
// resolve, and every preset resolves with every chain entry honoring its
// options. It returns a *ValidationError when any issue has error
// severity; Issues returns the warnings too.
func (c *Catalog) Validate() error {
	return issues(c.Issues()).asError("")
}

// Issues returns every issue Validate considers, warnings included, sorted by
// path.
func (c *Catalog) Issues() []Issue {
	found := c.check(true)
	sort.SliceStable(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found
}

// check validates the catalog. With full false, references that a lower
// layer may satisfy are not reported, and presets are not resolved.
func (c *Catalog) check(full bool) issues {
	var found issues
	switch {
	case c.Version == 0:
		found.errorf("version", CodeMissing, "version is required (current: %d)", SchemaVersion)
	case c.Version != SchemaVersion:
		found.errorf("version", CodeVersion, "unsupported catalog version %d (this reader supports %d, and %d through UpgradeV1)", c.Version, SchemaVersion, SchemaVersionV1)
	}
	if c.upgraded {
		found.warnf("version", WarnV1Catalog, "version 1 catalog upgraded in memory; run saige catalog migrate --write to store version 2")
	}
	for _, name := range sortedKeys(c.ModelTemplates) {
		t := c.ModelTemplates[name]
		if isNullRaw(t.raw) {
			continue
		}
		checkModelSpec("model_templates."+name, t, &found)
		c.modelChain("model_templates."+name, t, &found, full)
	}
	for _, name := range sortedKeys(c.OfferingTemplates) {
		t := c.OfferingTemplates[name]
		if isNullRaw(t.raw) {
			continue
		}
		if t.Model != "" || t.Endpoint != "" {
			found.errorf("offering_templates."+name, CodeBadValue, "model and endpoint belong on offerings, not templates")
		}
		checkOfferingSpec("offering_templates."+name, t, &found)
		c.offeringChain("offering_templates."+name, t, &found, full)
	}
	for _, k := range sortedKeys(c.Models) {
		m := c.Models[k]
		path := "models." + k
		if isNullRaw(m.raw) || m.Delete {
			continue
		}
		if _, _, ok := splitModelKey(k); !ok {
			found.errorf(path, CodeBadValue, "a model key is <vendor>/<prefix>")
		}
		checkModelSpec(path, m, &found)
		c.modelChain(path, m, &found, full)
	}
	c.checkEndpoints(&found, full)
	c.checkOfferings(&found, full)
	for _, name := range sortedKeys(c.Presets) {
		c.checkPresetShape("presets."+string(name), c.Presets[name], &found)
	}
	if c.Dials != nil {
		checkDialValues("dials", *c.Dials, &found)
	}
	if !full {
		return found
	}
	c.checkSuccessors(&found)
	if c.DefaultPreset != "" {
		if _, ok := c.Presets[c.DefaultPreset]; !ok {
			found.errorf("default_preset", CodeUnknownPreset, "unknown preset %q", c.DefaultPreset)
		}
	}
	for _, name := range sortedKeys(c.Presets) {
		_, presetIssues := c.resolve(name)
		found = append(found, presetIssues...)
	}
	return found
}

// checkModelSpec validates the names and values a model declares.
func checkModelSpec(path string, s ModelSpec, found *issues) {
	if s.Tier != "" && s.Tier != TierFrontier && s.Tier != TierStandard && s.Tier != TierEconomy {
		found.errorf(path+".tier", CodeBadValue, "tier must be frontier, standard or economy")
	}
	if l := s.Limits; l != nil {
		for name, v := range map[string]*int{"context_window": l.ContextWindow, "max_output_tokens": l.MaxOutputTokens} {
			if v != nil && *v < 0 {
				found.errorf(path+".limits."+name, CodeBadValue, "must not be negative")
			}
		}
	}
	if m := s.Modalities; m != nil {
		checkModalityNames(path+".modalities.in", m.In, found)
		checkModalityNames(path+".modalities.out", m.Out, found)
	}
}

func checkModalityNames(path string, ms []types.Modality, found *issues) {
	for i, m := range ms {
		if !slices.Contains(types.KnownModalities(), m) {
			found.errorf(fmt.Sprintf("%s[%d]", path, i), CodeModality, "unknown modality %q", m)
		}
	}
}

// knownParams lists the parameter names an offering may declare.
func knownParams() []types.ParamName {
	return []types.ParamName{types.ParamTemperature, types.ParamTopP, types.ParamTopK, types.ParamFrequencyPenalty,
		types.ParamPresencePenalty, types.ParamSeed, types.ParamMaxOutputTokens, types.ParamStop, types.ParamParallelTools,
		types.ParamToolChoice, types.ParamReasoningEnabled, types.ParamReasoningEffort, types.ParamReasoningBudget,
		types.ParamServiceTier, types.ParamPromptCacheRetention}
}

// checkOfferingSpec validates the names and values one offering, template
// or override declares.
//
//nolint:gocyclo // one check per field of the file form
func checkOfferingSpec(path string, s OfferingSpec, found *issues) {
	known := types.KnownCapabilities()
	for field, list := range map[string][]types.Capability{
		"features": s.Features, "add_features": s.AddFeatures, "remove_features": s.RemoveFeatures,
	} {
		for i, c := range list {
			fp := fmt.Sprintf("%s.%s[%d]", path, field, i)
			switch {
			case !slices.Contains(known, c):
				found.errorf(fp, CodeUnknownCap, "unknown capability %q%s", c, hint(string(c), capNames()))
			case !types.IsFeature(c):
				p, _ := types.CapabilityParam(c)
				found.errorf(fp, CodeUnknownCap, "%q is a request parameter: declare params.%s", c, p)
			}
		}
	}
	for _, name := range sortedKeys(s.Params) {
		p := s.Params[name]
		pp := path + ".params." + string(name)
		if !slices.Contains(knownParams(), name) {
			found.errorf(pp, CodeParam, "unknown parameter %q", name)
		}
		if p == nil {
			continue
		}
		checkParamSpec(pp, *p, found)
	}
	for _, k := range sortedKeys(s.Constraints) {
		cn := s.Constraints[k]
		if cn == nil {
			continue
		}
		cp := path + ".constraints." + k
		for _, p := range append(append(slices.Clone(cn.Forbid), cn.Exclusive...), slices.Collect(maps.Keys(cn.Require))...) {
			if !slices.Contains(knownParams(), p) && p != types.CondRequestTools {
				found.errorf(cp, CodeParam, "unknown parameter %q", p)
			}
		}
		for key := range cn.When {
			if !slices.Contains(knownParams(), types.ParamName(key)) && !strings.HasPrefix(key, "request.") {
				found.errorf(cp+".when."+key, CodeParam, "unknown parameter %q", key)
			}
		}
	}
	if m := s.Modalities; m != nil {
		checkLimits(path+".modalities.in", m.In, found)
		checkLimits(path+".modalities.out", m.Out, found)
		for _, k := range sortedKeys(m.ToolResult) {
			switch m.ToolResult[k] {
			case "", types.ToolResultInline, types.ToolResultFollowUpUser, types.ToolResultNone:
			default:
				found.errorf(path+".modalities.tool_result."+string(k), CodeModality, "tool_result must be inline, follow_up_user or none")
			}
		}
	}
	if s.StructuredOutput != nil {
		switch *s.StructuredOutput {
		case types.StructuredOutputNone, types.StructuredOutputNative, types.StructuredOutputToolCall:
		default:
			found.errorf(path+".structured_output", CodeBadValue, "structured_output must be \"\", \"native\" or \"tool_call\"")
		}
	}
	for i, k := range s.ServerTools {
		if !slices.Contains(types.KnownServerToolKinds(), k) {
			found.errorf(fmt.Sprintf("%s.server_tools[%d]", path, i), CodeUnknownTool, "unknown server tool %q", k)
		}
	}
	checkFees(path, s.ServerToolFees, found)
	if s.Pricing != nil {
		checkPricing(path+".pricing", *s.Pricing, found)
	}
	for _, t := range sortedKeys(s.Tiers) {
		ts := s.Tiers[t]
		tp := path + ".tiers." + string(t)
		switch t {
		case types.ServicePriority, types.ServiceFlex, types.ServiceBatch:
		default:
			found.errorf(tp, CodeTier, "tier must be priority, flex or batch (standard is pricing)")
		}
		if ts == nil {
			continue
		}
		if ts.Discount < 0 || ts.Discount >= 1 {
			found.errorf(tp+".discount", CodePricing, "discount must be at least 0 and below 1")
		}
		if ts.CachedInputPerMTok < 0 {
			found.errorf(tp+".cached_input_per_mtok", CodePricing, "rates must not be negative")
		}
		switch ts.Transport {
		case "", types.TransportBatch:
		default:
			found.errorf(tp+".transport", CodeTier, "transport must be empty or batch")
		}
		if t == types.ServiceBatch && ts.Transport != types.TransportBatch {
			found.errorf(tp+".transport", CodeTier, "the batch tier's transport is batch")
		}
		if ts.Pricing != nil {
			checkPricing(tp+".pricing", *ts.Pricing, found)
		}
	}
	for _, m := range sortedKeys(s.ModalityPricing) {
		if r := s.ModalityPricing[m]; r != nil && (r.InputPerMTok < 0 || r.OutputPerMTok < 0) {
			found.errorf(path+".modality_pricing."+string(m), CodePricing, "rates must not be negative")
		}
	}
	if s.Defaults != nil {
		checkOptionsShape(path+".defaults", s.Defaults, found)
	}
	checkDialsShape(path+".dials", s.Dials, found)
	if f := s.Fallback; f != nil {
		for i, id := range append(slices.Clone(f.Equivalents), f.LargerContext...) {
			if _, _, ok := ParseOfferingID(id); !ok {
				found.errorf(fmt.Sprintf("%s.fallback[%d]", path, i), CodeBadValue, "a fallback hint is <vendor>/<model>@<endpoint>, got %q", id)
			}
		}
	}
}

func checkParamSpec(path string, p ParamSpec, found *issues) {
	switch p.Type {
	case "", types.ParamTypeNumber, types.ParamTypeInteger, types.ParamTypeBoolean, types.ParamTypeEnum, types.ParamTypeStringList:
	default:
		found.errorf(path+".type", CodeParam, "type must be number, integer, boolean, enum or string_list")
	}
	if p.Min != nil && p.Max != nil && *p.Min > *p.Max {
		found.errorf(path, CodeParam, "min %g exceeds max %g", *p.Min, *p.Max)
	}
}

func checkLimits(path string, in map[types.Modality]*ModalityLimitSpec, found *issues) {
	for _, m := range sortedKeys(in) {
		lp := path + "." + string(m)
		if !slices.Contains(types.KnownModalities(), m) {
			found.errorf(lp, CodeModality, "unknown modality %q", m)
		}
		l := in[m]
		if l == nil {
			continue
		}
		for i, mt := range l.Media {
			mp := fmt.Sprintf("%s.media[%d]", lp, i)
			switch {
			case !slices.Contains(types.KnownMediaTypes(), mt):
				found.errorf(mp, CodeUnknownMedia, "unknown media type %q", mt)
			case mt.Modality() != m:
				found.errorf(mp, CodeModality, "%s is %s, not %s", mt, mt.Modality(), m)
			}
		}
		for i, sk := range l.Sources {
			switch sk {
			case types.SourceInline, types.SourceURI, types.SourceFile:
			default:
				found.errorf(fmt.Sprintf("%s.sources[%d]", lp, i), CodeModality, "source must be inline, uri or file")
			}
		}
		for name, v := range map[string]int64{"max_bytes": deref64(l.MaxBytes), "max_count": int64(derefInt(l.MaxCount)),
			"max_pixels": int64(derefInt(l.MaxPixels)), "max_pages": int64(derefInt(l.MaxPages))} {
			if v < 0 {
				found.errorf(lp+"."+name, CodeBadValue, "must not be negative")
			}
		}
	}
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func checkFees(path string, fees map[types.ServerToolKind]Fee, found *issues) {
	for _, k := range sortedKeys(fees) {
		f := fees[k]
		fp := path + ".server_tool_fees." + string(k)
		if !slices.Contains(types.KnownServerToolKinds(), k) {
			found.errorf(fp, CodeUnknownTool, "unknown server tool %q", k)
		}
		if f.PerUse < 0 {
			found.errorf(fp+".per_use", CodePricing, "fees must not be negative")
		}
		if f.PerUse > 0 && f.AsOf == "" {
			found.errorf(fp+".as_of", CodePricing, "as_of is required when a fee is set")
		}
	}
}

func checkPricing(path string, p PricingSpec, found *issues) {
	rates := []float64{p.InputPerMTok, p.OutputPerMTok, p.CachedInputPerMTok, p.CacheWritePerMTok, p.PerRequest}
	if slices.ContainsFunc(rates, func(r float64) bool { return r < 0 }) {
		found.errorf(path, CodePricing, "rates must not be negative")
	}
	if (slices.ContainsFunc(rates, func(r float64) bool { return r != 0 }) || p.Free) && p.AsOf == "" {
		found.errorf(path+".as_of", CodePricing, "as_of is required when a rate is set")
	}
}

// checkEndpoints validates every endpoint.
func (c *Catalog) checkEndpoints(found *issues, full bool) {
	primaries := map[types.ProviderName]string{}
	for _, name := range sortedKeys(c.Endpoints) {
		ep := c.Endpoints[name]
		path := "endpoints." + name
		if isNullRaw(ep.raw) {
			continue
		}
		// An overlay may patch one field of an endpoint a lower layer
		// declares, so the required fields are checked on the merged
		// catalog.
		if ep.Surface == "" && full {
			found.errorf(path+".surface", CodeMissing, "surface is required")
		} else if _, ok := SurfaceProvider(ep.Surface); !ok && ep.Surface != "" {
			found.warnf(path+".surface", CodeSurface, "surface %q is not one this SDK has an adapter for", ep.Surface)
		}
		if len(ep.Serves) == 0 && full {
			found.errorf(path+".serves", CodeMissing, "serves is required")
		}
		if ep.Primary {
			for _, v := range ep.Serves {
				if other, dup := primaries[v]; dup {
					found.errorf(path+".primary", CodePrimary, "%s already has primary endpoint %q", v, other)
				}
				primaries[v] = name
			}
		}
		if a := ep.Auth; a != nil {
			switch a.Type {
			case "", authAPIKey, authADC, authNone:
			default:
				found.errorf(path+".auth.type", CodeBadValue, "auth type must be api_key, adc or none")
			}
			if a.Secret != "" && !isSecretRef(a.Secret) {
				found.errorf(path+".auth.secret", CodeSecretRef, "a secret is a reference such as \"env:NAME\", never the credential")
			}
			if a.Type == authNone && a.Secret != "" {
				found.errorf(path+".auth", CodeBadValue, "auth type none takes no secret")
			}
		}
		if ep.Overrides != nil {
			checkOfferingSpec(path+".overrides", *ep.Overrides, found)
			if ep.Overrides.Model != "" || ep.Overrides.Endpoint != "" {
				found.errorf(path+".overrides", CodeBadValue, "overrides apply to every offering; they name no model or endpoint")
			}
			c.offeringChain(path+".overrides", *ep.Overrides, found, full)
		}
		if !full {
			continue
		}
		if t := ep.DefaultOfferingTemplate; t != "" {
			if _, ok := c.OfferingTemplates[t]; !ok {
				found.errorf(path+".default_offering_template", CodeUnknownTemplate, "unknown offering template %q", t)
			}
		}
		if src := ep.InheritOfferings; src != "" {
			seen := map[string]bool{name: true}
			for next := src; next != ""; next = c.Endpoints[next].InheritOfferings {
				if _, ok := c.Endpoints[next]; !ok {
					found.errorf(path+".inherit_offerings", CodeUnknownEndpoint, "unknown endpoint %q", next)
					break
				}
				if seen[next] {
					found.errorf(path+".inherit_offerings", CodeExtendsCycle, "inherit_offerings loops at %q", next)
					break
				}
				seen[next] = true
			}
		}
	}
}

// isSecretRef reports whether s references a secret rather than holding
// one.
func isSecretRef(s string) bool {
	for _, scheme := range []string{"env:", "op://", "file:"} {
		if strings.HasPrefix(s, scheme) && len(s) > len(scheme) {
			return true
		}
	}
	return false
}

// checkOfferings validates every explicit offering.
func (c *Catalog) checkOfferings(found *issues, full bool) {
	seen := map[string]int{}
	var ix *index
	if full {
		var ignored issues
		ix = c.index(&ignored, true)
	}
	for i, o := range c.Offerings {
		path := fmt.Sprintf("offerings[%d]", i)
		if o.Model == "" {
			found.errorf(path+".model", CodeMissing, "model is required")
		}
		if o.Endpoint == "" {
			found.errorf(path+".endpoint", CodeMissing, "endpoint is required")
		}
		k := OfferingID(o.Model, o.Endpoint)
		if j, dup := seen[k]; dup {
			found.errorf(path, CodeDuplicateOf, "offering %s is already declared at offerings[%d]", k, j)
		}
		seen[k] = i
		checkOfferingSpec(path, o, found)
		if o.Delete || o.Replace {
			continue
		}
		c.offeringChain(path, o, found, full)
		if !full {
			continue
		}
		vendor, _, ok := splitModelKey(o.Model)
		m, declared := c.Models[o.Model]
		if !ok || !declared || m.Delete {
			found.errorf(path+".model", CodeUnknownOffering, "unknown model %q", o.Model)
		}
		ep, ok := c.Endpoints[o.Endpoint]
		if !ok {
			found.errorf(path+".endpoint", CodeUnknownEndpoint, "unknown endpoint %q", o.Endpoint)
			continue
		}
		if !slices.Contains(ep.Serves, vendor) {
			found.errorf(path+".endpoint", CodeBadValue, "endpoint %q does not serve %s", o.Endpoint, vendor)
		}
		r := ix.offerings[o.Endpoint][o.Model]
		if r == nil {
			continue
		}
		for _, t := range sortedKeys(r.flat.Tiers) {
			if ts := r.flat.Tiers[t]; ts.Transport == types.TransportBatch && (ep.Modes == nil || !ep.Modes.Batch) {
				found.errorf(path+".tiers."+string(t), CodeTier, "the %s tier needs the batch mode endpoint %q does not declare", t, o.Endpoint)
			}
		}
		e := Entry{Provider: vendor, Prefix: r.off.Model.Prefix, Caps: r.off.Capabilities(), Dials: r.flat.Dials}
		checkRowDials(path, e, found)
	}
}

// checkSuccessors requires every superseded_by to resolve within the
// vendor, and forbids cycles.
func (c *Catalog) checkSuccessors(found *issues) {
	v := c.view()
	for _, k := range sortedKeys(c.Models) {
		vendor, prefix, ok := splitModelKey(k)
		if !ok || c.Models[k].Delete {
			continue
		}
		e, ok := v.match(vendor, string(prefix))
		if !ok || e.SupersededBy == "" {
			continue
		}
		path := "models." + k + ".superseded_by"
		seen := map[types.ModelID]bool{e.Prefix: true}
		for next := e.SupersededBy; next != ""; {
			row, ok := v.match(vendor, string(next))
			if !ok {
				found.errorf(path, CodeUnknownSuccessor, "successor %q matches no %s model", next, vendor)
				break
			}
			if seen[row.Prefix] {
				found.errorf(path, CodeSuccessorCycle, "successor chain loops at %q", row.Prefix)
				break
			}
			seen[row.Prefix] = true
			next = row.SupersededBy
		}
	}
}
