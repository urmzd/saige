package catalog

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// DialsSpec declares how a model row compiles model-neutral dials. Every
// part is optional: what a row leaves out is derived from its other
// declarations (see types.ModelCapabilities.EffectiveDialMap). Templates
// and rows merge it field by field, and level and depth maps key by key.
type DialsSpec struct {
	Creativity *CreativityDialSpec `json:"creativity,omitempty"`
	Reasoning  *ReasoningDialSpec  `json:"reasoning,omitempty"`
	Cache      *CacheDialSpec      `json:"cache,omitempty"`
	// Defaults are the model's own dials, applied below the preset's.
	Defaults *types.Dials `json:"defaults,omitempty"`
}

// CreativityDialSpec maps creativity levels to sampling options.
type CreativityDialSpec struct {
	Levels map[types.Creativity]OptionsSpec `json:"levels,omitempty"`
	// RequiresReasoningOff drops creativity whenever reasoning is active.
	RequiresReasoningOff *bool `json:"requires_reasoning_off,omitempty"`
}

// ReasoningDialSpec maps the reasoning dial. An empty object for off, on
// or adaptive means the mode is expressed by sending nothing.
type ReasoningDialSpec struct {
	Off      *OptionsSpec                `json:"off,omitempty"`
	On       *OptionsSpec                `json:"on,omitempty"`
	Adaptive *OptionsSpec                `json:"adaptive,omitempty"`
	Depth    map[types.Depth]OptionsSpec `json:"depth,omitempty"`
	// DepthChange is "per_request" when a depth change applies even inside
	// a tool loop with signed reasoning; empty waits for the next user turn.
	DepthChange       string `json:"depth_change,omitempty"`
	ChangeResetsCache *bool  `json:"change_resets_cache,omitempty"`
	// WithTools replaces the compiled reasoning when the request offers
	// tools, keyed by API surface ("chat" or "responses").
	WithTools map[string]OptionsSpec `json:"with_tools,omitempty"`
}

// CacheDialSpec names the prompt cache the cache dial turns on.
type CacheDialSpec struct {
	On *PromptCacheSpec `json:"on,omitempty"`
}

// merge layers over onto s, field by field and map key by map key.
func (s *DialsSpec) merge(over *DialsSpec) *DialsSpec {
	if over == nil {
		return s.clone()
	}
	out := s.clone()
	if out == nil {
		out = &DialsSpec{}
	}
	if c := over.Creativity; c != nil {
		if out.Creativity == nil {
			out.Creativity = &CreativityDialSpec{}
		}
		out.Creativity.Levels = mergeSpecMap(out.Creativity.Levels, c.Levels)
		if c.RequiresReasoningOff != nil {
			out.Creativity.RequiresReasoningOff = ptr(*c.RequiresReasoningOff)
		}
	}
	if r := over.Reasoning; r != nil {
		if out.Reasoning == nil {
			out.Reasoning = &ReasoningDialSpec{}
		}
		o := out.Reasoning
		for _, f := range []struct {
			dst **OptionsSpec
			src *OptionsSpec
		}{{&o.Off, r.Off}, {&o.On, r.On}, {&o.Adaptive, r.Adaptive}} {
			if f.src != nil {
				*f.dst = f.src.clone()
			}
		}
		o.Depth = mergeSpecMap(o.Depth, r.Depth)
		o.WithTools = mergeSpecMap(o.WithTools, r.WithTools)
		if r.DepthChange != "" {
			o.DepthChange = r.DepthChange
		}
		if r.ChangeResetsCache != nil {
			o.ChangeResetsCache = ptr(*r.ChangeResetsCache)
		}
	}
	if over.Cache != nil {
		out.Cache = over.Cache.clone()
	}
	if over.Defaults != nil {
		var base types.Dials
		if out.Defaults != nil {
			base = *out.Defaults
		}
		d := base.Merge(*over.Defaults)
		out.Defaults = &d
	}
	return out
}

func mergeSpecMap[K comparable](base, over map[K]OptionsSpec) map[K]OptionsSpec {
	if base == nil && over == nil {
		return nil
	}
	out := make(map[K]OptionsSpec, len(base)+len(over))
	for k, v := range base {
		out[k] = *v.clone()
	}
	for k, v := range over {
		out[k] = *v.clone()
	}
	return out
}

func (s *DialsSpec) clone() *DialsSpec {
	if s == nil {
		return nil
	}
	out := &DialsSpec{Cache: s.Cache.clone()}
	if c := s.Creativity; c != nil {
		out.Creativity = &CreativityDialSpec{Levels: mergeSpecMap(c.Levels, nil)}
		if c.RequiresReasoningOff != nil {
			out.Creativity.RequiresReasoningOff = ptr(*c.RequiresReasoningOff)
		}
	}
	if r := s.Reasoning; r != nil {
		out.Reasoning = &ReasoningDialSpec{Off: r.Off.clone(), On: r.On.clone(), Adaptive: r.Adaptive.clone(),
			Depth: mergeSpecMap(r.Depth, nil), DepthChange: r.DepthChange, WithTools: mergeSpecMap(r.WithTools, nil)}
		if r.ChangeResetsCache != nil {
			out.Reasoning.ChangeResetsCache = ptr(*r.ChangeResetsCache)
		}
	}
	if s.Defaults != nil {
		d := s.Defaults.Clone()
		out.Defaults = &d
	}
	return out
}

func (c *CacheDialSpec) clone() *CacheDialSpec {
	if c == nil {
		return nil
	}
	out := &CacheDialSpec{}
	if c.On != nil {
		pc := *c.On
		out.On = &pc
	}
	return out
}

// dialMap converts the declaration to the form the compiler reads.
func (s *DialsSpec) dialMap() types.DialMap {
	var m types.DialMap
	if s == nil {
		return m
	}
	toMap := func(in map[types.Creativity]OptionsSpec) map[types.Creativity]types.RequestOptions {
		if in == nil {
			return nil
		}
		out := map[types.Creativity]types.RequestOptions{}
		for k, v := range in {
			out[k] = v.requestOptions()
		}
		return out
	}
	if c := s.Creativity; c != nil {
		m.Creativity = &types.CreativityMap{Levels: toMap(c.Levels), RequiresReasoningOff: c.RequiresReasoningOff != nil && *c.RequiresReasoningOff}
	}
	if r := s.Reasoning; r != nil {
		rm := &types.ReasoningMap{DepthChange: r.DepthChange, ChangeResetsCache: r.ChangeResetsCache != nil && *r.ChangeResetsCache}
		ro := func(o *OptionsSpec) *types.RequestOptions {
			if o == nil {
				return nil
			}
			v := o.requestOptions()
			return &v
		}
		rm.Off, rm.On, rm.Adaptive = ro(r.Off), ro(r.On), ro(r.Adaptive)
		if r.Depth != nil {
			rm.Depth = map[types.Depth]types.RequestOptions{}
			for k, v := range r.Depth {
				rm.Depth[k] = v.requestOptions()
			}
		}
		if r.WithTools != nil {
			rm.WithTools = map[string]types.RequestOptions{}
			for k, v := range r.WithTools {
				rm.WithTools[k] = v.requestOptions()
			}
		}
		m.Reasoning = rm
	}
	if s.Cache != nil && s.Cache.On != nil {
		m.CacheMode = s.Cache.On.Mode
	}
	if s.Defaults != nil {
		m.Defaults = s.Defaults.Clone()
	}
	return m
}

// cacheSpec returns the prompt cache the cache dial turns on for a model:
// the row's declaration, or the derived mode with its usual settings.
func cacheSpec(s *DialsSpec, mc types.ModelCapabilities) *PromptCacheSpec {
	if s != nil && s.Cache != nil && s.Cache.On != nil {
		pc := *s.Cache.On
		return &pc
	}
	switch mc.EffectiveDialMap().CacheMode {
	case PromptCacheMarkers:
		return &PromptCacheSpec{Mode: PromptCacheMarkers, TTL: "5m", Tools: true, System: true}
	case PromptCacheAutomatic:
		return &PromptCacheSpec{Mode: PromptCacheAutomatic}
	}
	return nil
}

// dialUnsetPrefix marks an entry's unset name as a dial, as in
// "dials.creativity".
const dialUnsetPrefix = "dials."

// dialUnsetNames lists the dial names an entry may unset.
func dialUnsetNames() []string {
	var out []string
	for _, n := range types.AllDialNames() {
		out = append(out, dialUnsetPrefix+string(n))
	}
	return out
}

// unsetNames lists every name an entry may unset.
func unsetNames() []string { return append(optionNames(), dialUnsetNames()...) }

// dialScope maps a catalog layer to the dial scope reported in decisions.
func dialScope(l Layer) string {
	switch l {
	case LayerCatalog:
		return types.DialScopeGlobal
	case LayerModel:
		return types.DialScopeModel
	case LayerPreset:
		return types.DialScopePreset
	case LayerEntry:
		return types.DialScopeEntry
	}
	return string(l)
}

// layerOfScope is the inverse of dialScope.
func layerOfScope(scope string) Layer {
	switch scope {
	case types.DialScopeGlobal:
		return LayerCatalog
	case types.DialScopeModel:
		return LayerModel
	case types.DialScopePreset:
		return LayerPreset
	}
	return LayerEntry
}

// checkDialsShape validates the names in a dials declaration on its own.
func checkDialsShape(path string, s *DialsSpec, found *issues) {
	if s == nil {
		return
	}
	if c := s.Creativity; c != nil {
		for _, k := range slices.Sorted(maps.Keys(c.Levels)) {
			lp := path + ".creativity.levels." + string(k)
			if err := (types.Dials{Creativity: &k}).Validate(); err != nil {
				found.errorf(lp, CodeDial, "unknown creativity level %q", k)
			}
			o := c.Levels[k]
			checkOptionsShape(lp, &o, found)
		}
	}
	if r := s.Reasoning; r != nil {
		for name, o := range map[string]*OptionsSpec{"off": r.Off, "on": r.On, "adaptive": r.Adaptive} {
			if o != nil {
				checkOptionsShape(path+".reasoning."+name, o, found)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(r.Depth)) {
			dp := path + ".reasoning.depth." + string(k)
			if err := (types.Dials{Reasoning: &types.ReasoningDial{Depth: k}}).Validate(); err != nil {
				found.errorf(dp, CodeDial, "unknown depth %q", k)
			}
			o := r.Depth[k]
			checkOptionsShape(dp, &o, found)
		}
		for _, k := range slices.Sorted(maps.Keys(r.WithTools)) {
			if k != types.SurfaceChat && k != types.SurfaceResponses {
				found.errorf(path+".reasoning.with_tools."+k, CodeDial, "unknown API surface %q (chat or responses)", k)
			}
		}
		switch r.DepthChange {
		case "", types.DepthChangePerRequest:
		default:
			found.errorf(path+".reasoning.depth_change", CodeDial, "depth_change must be %q or empty", types.DepthChangePerRequest)
		}
	}
	if s.Cache != nil && s.Cache.On != nil {
		o := OptionsSpec{PromptCache: s.Cache.On}
		checkOptionsShape(path+".cache.on", &o, found)
	}
	if s.Defaults != nil {
		checkDialValues(path+".defaults", *s.Defaults, found)
	}
}

// checkDialValues validates dial values on their own.
func checkDialValues(path string, d types.Dials, found *issues) {
	if err := d.Validate(); err != nil {
		found.errorf(path, CodeDial, "%s", types.OptionReason(err))
	}
}

// checkRowDials checks every raw value a resolved row declares for a dial:
// each must pass the row's own validation and be expressible by its
// adapter, so a bad mapping fails at load time rather than on a request.
func checkRowDials(path string, e Entry, found *issues) {
	s := e.Dials
	if s == nil {
		return
	}
	mc := e.Caps.ForModel(string(e.Prefix))
	mc.Provider = string(e.Provider)
	check := func(p string, o types.RequestOptions) {
		if err := mc.ValidateOptions(o); err != nil {
			found.errorf(p, CodeDial, "%s for %s/%s", types.OptionReason(err), e.Provider, e.Prefix)
			return
		}
		if err := Expressible(e.Provider, o); err != nil {
			found.errorf(p, CodeDial, "%v", err)
		}
	}
	dm := mc.EffectiveDialMap()
	off := types.RequestOptions{}
	if dm.Reasoning != nil && dm.Reasoning.Off != nil {
		off = *dm.Reasoning.Off
	}
	if c := s.Creativity; c != nil {
		for _, k := range slices.Sorted(maps.Keys(c.Levels)) {
			// A level is checked with reasoning off, the state the
			// conflict rule sends it in.
			level := c.Levels[k]
			check(path+".dials.creativity.levels."+string(k), off.Merge(level.requestOptions()))
		}
	}
	if r := s.Reasoning; r != nil {
		for name, o := range map[string]*OptionsSpec{"off": r.Off, "on": r.On, "adaptive": r.Adaptive} {
			if o != nil {
				check(path+".dials.reasoning."+name, o.requestOptions())
			}
		}
		for _, k := range slices.Sorted(maps.Keys(r.Depth)) {
			o := r.Depth[k]
			check(path+".dials.reasoning.depth."+string(k), o.requestOptions())
		}
		for _, k := range slices.Sorted(maps.Keys(r.WithTools)) {
			o := r.WithTools[k]
			check(path+".dials.reasoning.with_tools."+k, o.requestOptions())
		}
	}
	if s.Cache != nil && s.Cache.On != nil {
		if err := ExpressiblePromptCache(e.Provider, s.Cache.On.Mode); err != nil {
			found.errorf(path+".dials.cache.on.mode", CodeDial, "%v", err)
		}
	}
	if s.Defaults != nil {
		checkDialValues(path+".dials.defaults", *s.Defaults, found)
	}
}

// resolveDials collects an entry's dial layers, lowest first, and applies
// its unset names. It returns the layers and the origin of each dial.
func (c *Catalog) resolveDials(path string, spec PresetSpec, es EntrySpec, row Entry, hasRow bool, found *issues) ([]types.DialLayer, map[types.DialName]Layer) {
	type src struct {
		layer Layer
		dials *types.Dials
	}
	var srcs []src
	if c.Dials != nil {
		srcs = append(srcs, src{LayerCatalog, c.Dials})
	}
	if hasRow && row.Dials != nil && row.Dials.Defaults != nil {
		srcs = append(srcs, src{LayerModel, row.Dials.Defaults})
	}
	if es.Inherit != inheritNone && spec.Dials != nil {
		srcs = append(srcs, src{LayerPreset, spec.Dials})
	}
	origin := map[types.DialName]Layer{}
	for _, s := range srcs {
		for _, n := range s.dials.Names() {
			origin[n] = s.layer
		}
	}
	var unset []types.DialName
	for j, n := range es.Unset {
		name, ok := strings.CutPrefix(n, dialUnsetPrefix)
		if !ok {
			continue
		}
		dn, up := types.DialName(name), fmt.Sprintf("%s.unset[%d]", path, j)
		switch {
		case es.Dials != nil && slices.Contains(es.Dials.Names(), dn):
			found.errorf(up, CodeUnset, "dial %q is both set and unset by this entry", name)
		case origin[dn] == "":
			found.warnf(up, CodeUnset, "dial %q is not inherited, so unset has no effect", name)
		default:
			unset = append(unset, dn)
			delete(origin, dn)
		}
	}
	var layers []types.DialLayer
	for _, s := range srcs {
		if d := s.dials.Without(unset...); !d.IsZero() {
			layers = append(layers, types.DialLayer{Scope: dialScope(s.layer), Dials: d})
		}
	}
	if es.Dials != nil && !es.Dials.IsZero() {
		layers = append(layers, types.DialLayer{Scope: types.DialScopeEntry, Dials: es.Dials.Clone()})
		for _, n := range es.Dials.Names() {
			origin[n] = LayerEntry
		}
	}
	return layers, origin
}

// inheritNone is the entry inherit value that ignores the preset's options
// and dials.
const inheritNone = "none"

// applyCacheDial turns on the row's prompt cache when the entry's dials ask
// for it. An explicit prompt_cache option wins over the dial.
func applyCacheDial(e *ResolvedEntry, row Entry) {
	d := mergedDials(e.Dials)
	if d.Cache == nil || !*d.Cache || e.PromptCache != nil {
		return
	}
	if pc := cacheSpec(row.Dials, e.Caps); pc != nil {
		e.PromptCache = pc
		e.Origin[optionPromptCache] = e.DialOrigin[types.DialCache]
	}
}

// mergedDials returns the dials an entry's layers resolve to.
func mergedDials(layers []types.DialLayer) types.Dials {
	var d types.Dials
	for _, l := range layers {
		d = d.Merge(l.Dials)
	}
	return d
}

// checkEntryDials compiles an entry's dials against its own model, as a
// request with no tools would. A contractual dial the model cannot honor
// is an error; an advisory one that is mapped or dropped is a warning.
func checkEntryDials(path string, e ResolvedEntry, found *issues) {
	if len(e.Dials) == 0 {
		return
	}
	model := modelKey(e.Provider, e.Model)
	eff, rep, err := types.ResolveDials(e.Caps, e.Options, types.DialContext{}, types.DialPolicy{}, e.Dials...)
	for _, d := range rep.Decisions {
		dp := path + ".dials." + string(d.Dial)
		from := origin(layerOfScope(d.Scope))
		switch d.Action {
		case types.DialRejected:
			found.errorf(dp, CodeDial, "dial %s %s cannot be honored by %s: %s (%s)", d.Dial, d.Requested, model, d.Reason, from)
		case types.DialMapped:
			found.warnf(dp, WarnDialMapped, "dial %s %s is sent as %s on %s: %s (%s)", d.Dial, d.Requested, d.Sent, model, d.Reason, from)
		case types.DialDropped:
			found.warnf(dp, WarnDialDropped, "dial %s %s is dropped on %s: %s (%s)", d.Dial, d.Requested, model, d.Reason, from)
		case types.DialRawOverride:
			found.warnf(dp, WarnDialOverridden, "dial %s %s is overridden by the raw option %s (%s)", d.Dial, d.Requested, d.Sent, from)
		}
	}
	if err != nil {
		if !slices.ContainsFunc(rep.Decisions, func(d types.DialDecision) bool { return d.Action == types.DialRejected }) {
			found.errorf(path+".dials", CodeDial, "%s for %s", types.OptionReason(err), model)
		}
		return
	}
	if err := Expressible(e.Provider, eff); err != nil {
		found.errorf(path+".dials", CodeNotExpressible, "%v", err)
	}
}
