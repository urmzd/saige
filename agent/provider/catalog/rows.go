package catalog

import (
	"fmt"
	"sort"

	"github.com/urmzd/saige/agent/types"
)

// chatToolsAny is the file spelling of types.ChatToolsAny, which is empty in
// memory; an empty field in a row means "inherit".
const chatToolsAny = "any"

// maxExtendsDepth bounds a template chain.
const maxExtendsDepth = 4

// rowKey is the identity of a model row.
func rowKey(provider, prefix string) string { return key(provider, prefix) }

// apply layers one spec onto a partially resolved entry. Set fields replace;
// capability lists replace, then add, then remove.
func (s ModelSpec) apply(e *Entry) {
	c := &e.Caps
	if c.Caps == nil {
		c.Caps = map[types.Capability]bool{}
	}
	if len(s.Capabilities) > 0 {
		c.Caps = map[types.Capability]bool{}
		for _, cp := range s.Capabilities {
			c.Caps[cp] = true
		}
	}
	for _, cp := range s.AddCapabilities {
		c.Caps[cp] = true
	}
	for _, cp := range s.RemoveCapabilities {
		delete(c.Caps, cp)
	}
	if s.Tier != "" {
		e.Tier = s.Tier
	}
	if s.SupersededBy != "" {
		e.SupersededBy = s.SupersededBy
	}
	switch s.ChatCompletionsTools {
	case "":
	case chatToolsAny:
		c.ChatCompletionsTools = types.ChatToolsAny
	default:
		c.ChatCompletionsTools = types.ChatCompletionsTools(s.ChatCompletionsTools)
	}
	if l := s.Limits; l != nil {
		setInt(&c.ContextWindow, l.ContextWindow)
		setInt(&c.MaxOutputTokens, l.MaxOutputTokens)
		setInt(&c.DefaultMaxOutputTokens, l.DefaultMaxOutputTokens)
	}
	if r := s.Reasoning; r != nil {
		if r.Efforts != nil {
			c.ReasoningEfforts = append([]string(nil), r.Efforts...)
		}
		if r.DefaultEffort != nil {
			c.DefaultReasoningEffort = *r.DefaultEffort
		}
		setBool(&c.ReasoningRequired, r.Required)
		setBool(&c.ReasoningDefaultEnabled, r.DefaultEnabled)
		setInt(&c.MinReasoningBudget, r.MinBudget)
		setInt(&c.MaxReasoningBudget, r.MaxBudget)
		setBool(&c.DynamicReasoningBudget, r.DynamicBudget)
		setBool(&c.ZeroReasoningBudget, r.ZeroBudget)
		if r.ForcedToolChoice != nil {
			c.RejectsForcedToolChoice = !*r.ForcedToolChoice
		}
		if r.SamplingRequiresNoReasoning != nil {
			c.SamplingRequiresNoReasoning = append([]types.Capability(nil), r.SamplingRequiresNoReasoning...)
		}
	}
	if s.StructuredOutput != nil {
		c.StructuredOutput = *s.StructuredOutput
	}
	if s.Media != nil {
		c.Media = types.ContentSupport{NativeTypes: map[types.MediaType]bool{}}
		for _, mt := range s.Media {
			c.Media.NativeTypes[mt] = true
		}
	}
	if s.ServerTools != nil {
		c.ServerTools = append([]types.ServerToolKind(nil), s.ServerTools...)
	}
	if s.ServerToolFees != nil {
		e.ServerToolFees = make(map[types.ServerToolKind]Fee, len(s.ServerToolFees))
		for k, v := range s.ServerToolFees {
			e.ServerToolFees[k] = v
		}
	}
	if s.Pricing != nil {
		c.Pricing = s.Pricing.pricing()
	}
	if s.Defaults != nil {
		e.Defaults = s.Defaults.clone()
	}
	if s.Dials != nil {
		e.Dials = e.Dials.merge(s.Dials)
		c.DialMap = e.Dials.dialMap()
	}
	if s.Notes != nil {
		c.Notes = append([]string(nil), s.Notes...)
	}
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

// chainOf returns the templates spec extends, root first. Missing templates
// are reported only when full is set, since a layer may extend a template a
// lower layer defines.
func (c *Catalog) chainOf(path string, s ModelSpec, found *issues, full bool) ([]ModelSpec, bool) {
	var chain []ModelSpec
	seen := map[string]bool{}
	next := s.Extends
	for depth := 0; next != ""; depth++ {
		if seen[next] {
			found.errorf(path+".extends", CodeExtendsCycle, "template chain loops at %q", next)
			return nil, false
		}
		if depth >= maxExtendsDepth {
			found.errorf(path+".extends", CodeExtendsDepth, "template chain is deeper than %d", maxExtendsDepth)
			return nil, false
		}
		seen[next] = true
		t, ok := c.Templates[next]
		if !ok {
			if full {
				found.errorf(path+".extends", CodeUnknownTemplate, "unknown template %q", next)
			}
			return nil, false
		}
		chain = append([]ModelSpec{t}, chain...)
		next = t.Extends
	}
	return chain, true
}

// resolveSpec resolves one row or baseline through its templates.
func (c *Catalog) resolveSpec(path string, s ModelSpec, found *issues, full bool) (Entry, bool) {
	chain, ok := c.chainOf(path, s, found, full)
	if !ok {
		return Entry{}, false
	}
	e := Entry{Provider: s.Provider, Prefix: s.Prefix}
	for _, t := range chain {
		t.apply(&e)
	}
	s.apply(&e)
	// A mode without the capability is not a declaration.
	if !e.Caps.Supports(types.CapStructuredOutput) {
		e.Caps.StructuredOutput = types.StructuredOutputNone
	}
	return cloneEntry(e), true
}

// view resolves the catalog's rows and baselines without installing them.
// Rows that fail to resolve are left out; check reports them.
func (c *Catalog) view() view {
	var found issues
	v := view{baselines: map[string]types.ModelCapabilities{}}
	for i, m := range c.Models {
		if e, ok := c.resolveSpec(fmt.Sprintf("models[%d]", i), m, &found, true); ok {
			v.entries = append(v.entries, e)
		}
	}
	sort.SliceStable(v.entries, func(i, j int) bool {
		return rowKey(v.entries[i].Provider, v.entries[i].Prefix) < rowKey(v.entries[j].Provider, v.entries[j].Prefix)
	})
	for p, b := range c.Baselines {
		b.Provider = p
		if e, ok := c.resolveSpec("baselines."+p, b, &found, true); ok {
			v.baselines[p] = e.Caps
		}
	}
	return v
}

// Lookup resolves a model against this catalog value, with the same rules as
// the package-level Lookup, without installing anything.
func (c *Catalog) Lookup(provider, model string) (types.ModelCapabilities, bool) {
	return c.view().lookup(provider, model)
}

// Describe returns the row of this catalog value that serves a model.
func (c *Catalog) Describe(provider, model string) (Entry, bool) {
	e, ok := c.view().match(provider, model)
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(e), true
}
