package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Layer names where an effective option came from, lowest precedence first.
// A per-request override at call time sits above all of them and is merged
// by the adapter; it is not part of a resolved entry.
type Layer string

const (
	// LayerProvider is the adapter's own default; nothing is sent.
	LayerProvider Layer = "provider"
	// LayerCatalog is the catalog's top-level dials.
	LayerCatalog Layer = "catalog"
	// LayerModel is the catalog row's defaults.
	LayerModel Layer = "model"
	// LayerPreset is the preset's shared options.
	LayerPreset Layer = "preset"
	// LayerEntry is the chain entry's own options.
	LayerEntry Layer = "entry"
	// LayerRequest is a per-call override.
	LayerRequest Layer = "request"
)

// ResolvedEntry is one chain entry with its effective configuration fully
// materialized. It carries no secrets: APIKeyEnv names a variable.
type ResolvedEntry struct {
	// ProfileID is "<preset>/<entry id>", the router profile ID.
	ProfileID string
	// ID is the entry ID within its preset.
	ID              string
	Provider, Model string
	// Caps are the catalog capabilities of this exact model.
	Caps types.ModelCapabilities
	// Options are the effective options the adapter is built with.
	Options types.RequestOptions
	// Origin maps each option name to the layer that set it.
	Origin map[string]Layer
	// Dials are the entry's dial layers, lowest first. The adapter compiles
	// them per request against its own model, under any request dials.
	Dials []types.DialLayer
	// DialOrigin maps each dial to the layer that set it.
	DialOrigin     map[types.DialName]Layer
	PromptCache    *PromptCacheSpec
	ServerTools    []types.ServerTool
	Retry          *RetrySpec
	AttemptTimeout time.Duration
	BaseURL        string
	APIKeyEnv      string
	// Vertex is set when a Google entry is served through Vertex AI.
	Vertex   *VertexSpec
	Optional bool
	// LocalFallback is EntrySpec.LocalFallback.
	LocalFallback bool
	// ConfigHash is the first 16 hex digits of a SHA-256 over the entry's
	// canonical configuration. Equal hashes mean identical requests.
	ConfigHash string
}

// ResolvedPreset is a preset with every entry resolved and validated.
type ResolvedPreset struct {
	Name            string
	CatalogRevision string
	Description     string
	Chain           []ResolvedEntry
	ToolChoice      *types.ToolChoice
	OutputMode      string
	LLMTimeout      time.Duration
	// Compaction is the preset's compaction strategy, nil when it sets none.
	Compaction *types.CompactConfig
	Routing    RoutingSpec
	Warnings   []Issue
}

// PresetNames returns the preset names, sorted.
func (c *Catalog) PresetNames() []string { return sortedKeys(c.Presets) }

// Resolve materializes a preset: it applies the precedence rules to every
// chain entry and validates each entry against its own model. An option an
// entry cannot honor is an error, never dropped; fix it in the file with
// unset, an entry override, or inherit "none". Errors are a
// *ValidationError; warnings are returned on the preset.
func (c *Catalog) Resolve(name string) (ResolvedPreset, error) {
	rp, found := c.resolve(name)
	if err := found.asError(""); err != nil {
		return ResolvedPreset{}, err
	}
	return rp, nil
}

// ResolveModel builds a one-entry preset from a model row's defaults, for a
// caller that names a model rather than a preset. An empty provider is
// inferred from the catalog.
func (c *Catalog) ResolveModel(provider, model string) (ResolvedPreset, error) {
	v := c.view()
	if provider == "" {
		p, ok := v.inferProvider(model)
		if !ok {
			return ResolvedPreset{}, &ValidationError{Issues: []Issue{{Path: "model", Code: CodeBadValue,
				Message: fmt.Sprintf("cannot infer a provider for model %q", model), Severity: SeverityError}}}
		}
		provider = p
	}
	id := provider + "/" + model
	spec := PresetSpec{Chain: []EntrySpec{{ID: id, Provider: provider, Model: model}}}
	rp, found := c.resolveSpec2(id, "models."+id, spec, v)
	if err := found.asError(""); err != nil {
		return ResolvedPreset{}, err
	}
	return rp, nil
}

// inferProvider is InferProvider over this view.
func (v view) inferProvider(model string) (string, bool) {
	if p, rest, found := strings.Cut(model, "/"); found && rest != "" {
		for _, e := range v.entries {
			if strings.EqualFold(e.Provider, p) {
				return e.Provider, true
			}
		}
	}
	provider, bestLen := "", -1
	want := normalize(model)
	for _, e := range v.entries {
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(e.Prefix)) {
			return e.Provider, true
		}
		p := normalize(e.Prefix)
		if strings.HasPrefix(want, p) && (len(p) > bestLen || (len(p) == bestLen && e.Provider < provider)) {
			provider, bestLen = e.Provider, len(p)
		}
	}
	return provider, bestLen >= 0
}

// flatten follows a preset's extends chain: the child copies its parent and
// replaces each top-level key it sets.
func (c *Catalog) flatten(name string, found *issues) (PresetSpec, bool) {
	path := "presets." + name
	p, ok := c.Presets[name]
	if !ok {
		found.errorf(path, CodeUnknownPreset, "unknown preset %q", name)
		return PresetSpec{}, false
	}
	stack := []PresetSpec{p}
	seen := map[string]bool{name: true}
	for cur := p; cur.Extends != ""; {
		if seen[cur.Extends] {
			found.errorf(path+".extends", CodePresetCycle, "preset chain loops at %q", cur.Extends)
			return PresetSpec{}, false
		}
		parent, ok := c.Presets[cur.Extends]
		if !ok {
			found.errorf(path+".extends", CodeUnknownPreset, "unknown preset %q", cur.Extends)
			return PresetSpec{}, false
		}
		seen[cur.Extends] = true
		stack = append(stack, parent)
		cur = parent
	}
	var out PresetSpec
	for i := len(stack) - 1; i >= 0; i-- {
		s := stack[i]
		if s.Description != "" {
			out.Description = s.Description
		}
		if s.Options != nil {
			out.Options = s.Options
		}
		if s.Dials != nil {
			out.Dials = s.Dials
		}
		if s.ToolChoice != "" {
			out.ToolChoice = s.ToolChoice
		}
		if s.OutputMode != "" {
			out.OutputMode = s.OutputMode
		}
		if s.LLMTimeout != 0 {
			out.LLMTimeout = s.LLMTimeout
		}
		if s.Compaction != nil {
			out.Compaction = s.Compaction
		}
		if s.Retry != nil {
			out.Retry = s.Retry
		}
		if s.Routing != nil {
			out.Routing = s.Routing
		}
		if s.RequireDeclared != nil {
			out.RequireDeclared = s.RequireDeclared
		}
		if s.Chain != nil {
			out.Chain = s.Chain
		}
	}
	return out, true
}

func (c *Catalog) resolve(name string) (ResolvedPreset, issues) {
	var found issues
	spec, ok := c.flatten(name, &found)
	if !ok {
		return ResolvedPreset{}, found
	}
	rp, more := c.resolveSpec2(name, "presets."+name, spec, c.view())
	return rp, append(found, more...)
}

// resolveSpec2 resolves a flattened preset against a view.
func (c *Catalog) resolveSpec2(name, path string, spec PresetSpec, v view) (ResolvedPreset, issues) {
	var found issues
	rp := ResolvedPreset{Name: name, CatalogRevision: c.Revision, Description: spec.Description,
		OutputMode: spec.OutputMode, LLMTimeout: time.Duration(spec.LLMTimeout)}
	if rp.OutputMode == "auto" {
		rp.OutputMode = ""
	}
	if spec.Compaction != nil {
		cc := spec.Compaction.Config()
		rp.Compaction = &cc
	}
	if spec.Routing != nil {
		rp.Routing = *spec.Routing
		rp.Routing.Required = append([]types.Capability(nil), spec.Routing.Required...)
	}
	tc, err := parseToolChoice(spec.ToolChoice)
	if err != nil {
		found.errorf(path+".tool_choice", CodeToolChoice, "%v", err)
	}
	rp.ToolChoice = tc
	if len(spec.Chain) == 0 {
		found.errorf(path+".chain", CodeEmptyChain, "a preset needs at least one chain entry")
		return rp, found
	}
	ids := map[string]int{}
	for i, es := range spec.Chain {
		ep := fmt.Sprintf("%s.chain[%d]", path, i)
		e := c.resolveEntry(name, ep, spec, es, v, &found)
		if j, dup := ids[e.ID]; dup {
			found.errorf(ep+".id", CodeDuplicateEntry, "entry id %q is already used by chain[%d]", e.ID, j)
		}
		ids[e.ID] = i
		c.checkEntry(ep, spec, rp, e, &found)
		if i > 0 {
			primary := rp.Chain[0]
			if pw, w := primary.Caps.ContextWindow, e.Caps.ContextWindow; pw > 0 && w > 0 && w < pw {
				found.warnf(ep, WarnSmallerWindow, "context window %d is smaller than the primary's %d; long conversations cannot fail over here", w, pw)
			}
		}
		rp.Chain = append(rp.Chain, e)
	}
	if len(rp.Chain) > 1 {
		p := rp.Chain[0]
		if p.Caps.Supports(types.CapReasoningSignature) && p.Caps.ReasoningActive(p.Options) {
			found.warnf(path+".chain[0]", WarnSignedReasoning, "the primary signs its reasoning, so a tool loop it starts cannot fail over to another entry")
		}
	}
	for _, is := range found {
		if is.Severity == SeverityWarning {
			rp.Warnings = append(rp.Warnings, is)
		}
	}
	return rp, found
}

// layered is one option source in precedence order.
type layered struct {
	layer Layer
	spec  *OptionsSpec
}

// resolveEntry applies the precedence rules to one chain entry.
func (c *Catalog) resolveEntry(preset, path string, spec PresetSpec, es EntrySpec, v view, found *issues) ResolvedEntry {
	e := ResolvedEntry{ID: es.ID, Provider: es.Provider, Model: es.Model, BaseURL: es.BaseURL,
		APIKeyEnv: es.APIKeyEnv, Optional: es.Optional, LocalFallback: es.LocalFallback,
		AttemptTimeout: time.Duration(es.AttemptTimeout), Origin: map[string]Layer{}}
	if es.LocalFallback && es.Provider != "ollama" {
		found.errorf(path+".local_fallback", CodeBadValue, "local_fallback applies only to ollama entries")
	}
	if es.Vertex != nil {
		vs := *es.Vertex
		e.Vertex = &vs
		if es.Provider != "google" {
			found.errorf(path+".vertex", CodeBadValue, "vertex applies only to google entries")
		}
	}
	if e.ID == "" {
		e.ID = es.Provider + "/" + es.Model
	}
	e.ProfileID = preset + "/" + e.ID
	if preset == e.ID {
		e.ProfileID = e.ID
	}
	e.Caps, _ = v.lookup(es.Provider, es.Model)
	row, hasRow := v.match(es.Provider, es.Model)

	var layers []layered
	if hasRow && row.Defaults != nil {
		layers = append(layers, layered{LayerModel, row.Defaults})
	}
	if es.Inherit != inheritNone && spec.Options != nil {
		layers = append(layers, layered{LayerPreset, spec.Options})
	}
	if es.Options != nil {
		layers = append(layers, layered{LayerEntry, es.Options})
	}
	var opts types.RequestOptions
	for _, l := range layers {
		o := l.spec.requestOptions()
		opts = opts.Merge(o)
		for _, n := range o.OptionNames() {
			e.Origin[n] = l.layer
		}
		if l.spec.PromptCache != nil {
			pc := *l.spec.PromptCache
			e.PromptCache = &pc
			e.Origin[optionPromptCache] = l.layer
		}
		if l.spec.ServerTools != nil {
			e.ServerTools = nil
			for _, st := range l.spec.ServerTools {
				e.ServerTools = append(e.ServerTools, st.serverTool())
			}
			e.Origin[optionServerTools] = l.layer
		}
	}
	entrySets := map[string]bool{}
	if es.Options != nil {
		for _, n := range es.Options.requestOptions().OptionNames() {
			entrySets[n] = true
		}
		if es.Options.PromptCache != nil {
			entrySets[optionPromptCache] = true
		}
		if es.Options.ServerTools != nil {
			entrySets[optionServerTools] = true
		}
	}
	for j, n := range es.Unset {
		up := fmt.Sprintf("%s.unset[%d]", path, j)
		switch {
		case entrySets[n]:
			found.errorf(up, CodeUnset, "option %q is both set and unset by this entry", n)
			continue
		case e.Origin[n] == "":
			found.warnf(up, CodeUnset, "option %q is not inherited, so unset has no effect", n)
			continue
		}
		delete(e.Origin, n)
		switch n {
		case optionPromptCache:
			e.PromptCache = nil
		case optionServerTools:
			e.ServerTools = nil
		default:
			opts = opts.Without(n)
		}
	}
	if opts.MaxOutputTokens == nil && e.Caps.DefaultMaxOutputTokens > 0 {
		e.Origin[types.OptionMaxOutputTokens] = LayerProvider
	}
	e.Options = opts
	e.Dials, e.DialOrigin = c.resolveDials(path, spec, es, row, hasRow, found)
	applyCacheDial(&e, row)

	e.Retry = spec.Retry
	if es.Retry != nil {
		e.Retry = es.Retry
	}
	if e.Retry != nil {
		r := *e.Retry
		e.Retry = &r
	}
	e.ConfigHash = configHash(e)
	return e
}

// origin describes where an option came from, for messages.
func origin(l Layer) string {
	switch l {
	case LayerModel:
		return "from model defaults"
	case LayerPreset:
		return "inherited from preset options"
	case LayerEntry:
		return "set by this entry"
	case LayerProvider:
		return "provider default"
	}
	return "unknown origin"
}

// checkEntry validates one resolved entry against its own model.
func (c *Catalog) checkEntry(path string, spec PresetSpec, rp ResolvedPreset, e ResolvedEntry, found *issues) {
	model := e.Provider + "/" + e.Model
	optPath := func(name string) string { return path + ".options." + name }
	fail := func(name, code, reason string) {
		found.errorf(optPath(name), code, "%s for %s (%s)", reason, model, origin(e.Origin[name]))
	}
	if strings.TrimSpace(e.Model) == "" || e.Provider == "" {
		return
	}
	if !e.Caps.Known {
		if spec.RequireDeclared != nil && *spec.RequireDeclared {
			found.errorf(path+".model", CodeNotDeclared, "%s is not an exact catalog row and the preset requires declared models", model)
		} else if e.Caps.Family != "" {
			found.warnf(path+".model", WarnInferredModel, "%s is inferred from family %s, not an exact row", model, e.Caps.Family)
		} else {
			found.warnf(path+".model", WarnInferredModel, "%s is not in the catalog; the %s baseline applies", model, e.Provider)
		}
	}
	if succ, ok := c.successor(e.Provider, e.Model); ok {
		found.warnf(path+".model", WarnSuperseded, "%s is superseded by %s", model, succ)
	}
	if err := e.Caps.ValidateOptions(e.Options); err != nil {
		name := culprit(e.Caps, e.Options)
		fail(name, CodeUnsupported, optionReason(err))
	} else if err := Expressible(e.Provider, e.Options); err != nil {
		var ee *ExpressError
		if errors.As(err, &ee) {
			name := ee.Option
			if strings.HasPrefix(name, "reasoning") {
				name = types.OptionReasoning
			}
			fail(name, CodeNotExpressible, ee.Reason)
		}
	}
	checkEntryDials(path, e, found)
	c.checkEntryExtras(path, rp, e, fail, found)
}

// checkEntryExtras validates an entry's prompt cache, server tools, the
// preset-level tool choice and output mode, limits, and routing needs.
func (c *Catalog) checkEntryExtras(path string, rp ResolvedPreset, e ResolvedEntry, fail func(name, code, reason string), found *issues) {
	model := e.Provider + "/" + e.Model
	if pc := e.PromptCache; pc != nil && pc.Mode != PromptCacheOff {
		var err error
		switch {
		case ExpressiblePromptCache(e.Provider, pc.Mode) != nil:
			err = fmt.Errorf("mode %q is not expressible by the %s adapter", pc.Mode, e.Provider)
		case pc.Mode == PromptCacheMarkers && !e.Caps.Supports(types.CapPromptCacheMarkers):
			err = errors.New("prompt cache markers are not declared supported")
		case pc.Mode == PromptCacheAutomatic && !e.Caps.Supports(types.CapAutomaticPromptCache):
			err = errors.New("automatic prompt caching is not declared supported")
		}
		if err != nil {
			fail(optionPromptCache, CodePromptCache, err.Error())
		}
	}
	for _, st := range e.ServerTools {
		switch {
		case ExpressibleServerTools(e.Provider) != nil:
			fail(optionServerTools, CodeNotExpressible, "server tools are not sent by the "+e.Provider+" adapter")
		case !e.Caps.SupportsServerTool(st.Kind) || !e.Caps.Supports(st.Capability()):
			fail(optionServerTools, CodeServerTool, fmt.Sprintf("server tool %q is not declared supported", st.Kind))
		}
	}
	if rp.ToolChoice != nil && rp.ToolChoice.Mode != types.ToolChoiceAuto && !e.Caps.Supports(types.CapToolChoice) {
		found.errorf(path, CodeToolChoice, "preset tool_choice %q needs tool_choice support, which %s does not declare", rp.ToolChoice.Mode, model)
	}
	switch rp.OutputMode {
	case "native":
		if e.Caps.StructuredOutput != types.StructuredOutputNative || !e.Caps.Supports(types.CapStructuredOutput) {
			msg := fmt.Sprintf("output_mode native needs native structured output, which %s does not declare", model)
			if e.Caps.Supports(types.CapTools) {
				msg += "; use \"output_mode\": \"tool\""
			}
			found.errorf(path, CodeOutputMode, "%s", msg)
		}
	case "tool":
		if !e.Caps.Supports(types.CapTools) {
			found.errorf(path, CodeOutputMode, "output_mode tool needs tool calling, which %s does not declare", model)
		}
	}
	if n := e.Options.MaxOutputTokens; n != nil && e.Caps.ContextWindow > 0 && *n > int64(e.Caps.ContextWindow) {
		found.warnf(path+".options."+types.OptionMaxOutputTokens, WarnExceedsWindow, "max_output_tokens %d exceeds the context window %d of %s", *n, e.Caps.ContextWindow, model)
	}
	for _, cp := range rp.Routing.Required {
		if !e.Caps.Supports(cp) {
			found.errorf(path, CodeUnsupported, "routing requires %q, which %s does not declare", cp, model)
		}
	}
}

// successor follows superseded_by within this catalog.
func (c *Catalog) successor(provider, model string) (string, bool) {
	v := c.view()
	e, ok := v.match(provider, model)
	if !ok || e.SupersededBy == "" {
		return "", false
	}
	return e.SupersededBy, true
}

// culprit finds the option whose removal makes the options valid, so the
// error path names it. Interactions fall back to the first option set.
func culprit(caps types.ModelCapabilities, o types.RequestOptions) string {
	names := o.OptionNames()
	for _, n := range names {
		if caps.ValidateOptions(o.Without(n)) == nil {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return "options"
}

// optionReason extracts the reason from a capability validation error.
func optionReason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, types.ErrInvalidModelConfig.Error()+": "); i >= 0 {
		msg = msg[i+len(types.ErrInvalidModelConfig.Error())+2:]
	}
	if _, rest, ok := strings.Cut(msg, ": "); ok {
		msg = rest
	}
	return strings.TrimSuffix(msg, " for this model")
}

// hashedEntry is the canonical configuration ConfigHash covers. It holds no
// credentials.
type hashedEntry struct {
	Provider       string       `json:"provider"`
	Model          string       `json:"model"`
	Options        *OptionsSpec `json:"options"`
	BaseURL        string       `json:"base_url,omitempty"`
	APIKeyEnv      string       `json:"api_key_env,omitempty"`
	Vertex         *VertexSpec  `json:"vertex,omitempty"`
	Retry          *RetrySpec   `json:"retry,omitempty"`
	AttemptTimeout Duration     `json:"attempt_timeout,omitzero"`
	// Dials and Compiled are set only for an entry with dials, so the hash
	// of an entry without them is unchanged. Compiled is what the dials
	// compile to on the entry's model, so a changed mapping changes it.
	Dials    []types.DialLayer `json:"dials,omitempty"`
	Compiled *OptionsSpec      `json:"compiled,omitempty"`
}

func configHash(e ResolvedEntry) string {
	o := optionsSpec(e.Options)
	o.PromptCache = e.PromptCache
	for _, st := range e.ServerTools {
		o.ServerTools = append(o.ServerTools, serverToolSpec(st))
	}
	h := hashedEntry{Provider: e.Provider, Model: e.Model, Options: o, BaseURL: e.BaseURL,
		APIKeyEnv: e.APIKeyEnv, Vertex: e.Vertex, Retry: e.Retry, AttemptTimeout: Duration(e.AttemptTimeout)}
	if len(e.Dials) > 0 {
		h.Dials = e.Dials
		if eff, _, err := types.ResolveDials(e.Caps, e.Options, types.DialContext{}, types.DialPolicy{}, e.Dials...); err == nil {
			h.Compiled = optionsSpec(eff)
		}
	}
	data, _ := json.Marshal(h)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// HasLayer reports whether an option came from the given layer.
func (e ResolvedEntry) HasLayer(name string, l Layer) bool { return e.Origin[name] == l }

// OptionNames returns the entry's option names with an origin, sorted in
// canonical order.
func (e ResolvedEntry) OptionNames() []string {
	var out []string
	for _, n := range optionNames() {
		if _, ok := e.Origin[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// ProfileIDs returns the chain's profile IDs in failover order.
func (p ResolvedPreset) ProfileIDs() []string {
	out := make([]string, len(p.Chain))
	for i, e := range p.Chain {
		out[i] = e.ProfileID
	}
	return out
}

// Entry returns the chain entry with a profile ID.
func (p ResolvedPreset) Entry(profileID string) (ResolvedEntry, bool) {
	i := slices.IndexFunc(p.Chain, func(e ResolvedEntry) bool { return e.ProfileID == profileID })
	if i < 0 {
		return ResolvedEntry{}, false
	}
	return p.Chain[i], true
}
