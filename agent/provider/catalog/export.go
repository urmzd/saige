package catalog

import (
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// Export snapshots what Lookup resolves now, including rows added with
// Register and baselines set with RegisterBaseline, as a flat catalog: every
// row written out in full, with no templates. The presets, revision and
// default preset come from the active catalog. Loading the result and
// installing it reproduces the same lookups.
func Export() *Catalog {
	act := Active()
	mu.RLock()
	defer mu.RUnlock()
	out := &Catalog{Version: SchemaVersion, Revision: act.Revision, Presets: act.Presets, DefaultPreset: act.DefaultPreset}
	for _, e := range globalView().entries {
		out.Models = append(out.Models, entrySpec(e))
	}
	if len(baseline) > 0 {
		out.Baselines = map[string]ModelSpec{}
		for p, b := range baseline {
			out.Baselines[p] = baselineSpec(b)
		}
	}
	return out
}

// entrySpec writes a resolved entry as a self-contained row.
func entrySpec(e Entry) ModelSpec {
	c := e.Caps
	s := ModelSpec{Provider: e.Provider, Prefix: e.Prefix, Tier: e.Tier, SupersededBy: e.SupersededBy,
		ChatCompletionsTools: string(c.ChatCompletionsTools)}
	s.Capabilities = append(s.Capabilities, c.List()...)
	if len(s.Capabilities) == 0 {
		// An empty list reads as "inherit"; a row without a template has
		// nothing to inherit, so the result is the same.
		s.Capabilities = nil
	}
	limits := LimitsSpec{}
	if c.ContextWindow != 0 {
		limits.ContextWindow = ptr(c.ContextWindow)
	}
	if c.MaxOutputTokens != 0 {
		limits.MaxOutputTokens = ptr(c.MaxOutputTokens)
	}
	if c.DefaultMaxOutputTokens != 0 {
		limits.DefaultMaxOutputTokens = ptr(c.DefaultMaxOutputTokens)
	}
	if limits != (LimitsSpec{}) {
		s.Limits = &limits
	}
	r := ReasoningSpec{Efforts: append([]string(nil), c.ReasoningEfforts...),
		SamplingRequiresNoReasoning: append([]types.Capability(nil), c.SamplingRequiresNoReasoning...)}
	if c.DefaultReasoningEffort != "" {
		r.DefaultEffort = ptr(c.DefaultReasoningEffort)
	}
	if c.ReasoningRequired {
		r.Required = ptr(true)
	}
	if c.ReasoningDefaultEnabled {
		r.DefaultEnabled = ptr(true)
	}
	if c.MinReasoningBudget != 0 {
		r.MinBudget = ptr(c.MinReasoningBudget)
	}
	if c.MaxReasoningBudget != 0 {
		r.MaxBudget = ptr(c.MaxReasoningBudget)
	}
	if c.DynamicReasoningBudget {
		r.DynamicBudget = ptr(true)
	}
	if c.ZeroReasoningBudget {
		r.ZeroBudget = ptr(true)
	}
	if c.RejectsForcedToolChoice {
		r.ForcedToolChoice = ptr(false)
	}
	if len(r.Efforts) > 0 || len(r.SamplingRequiresNoReasoning) > 0 || r.DefaultEffort != nil || r.Required != nil ||
		r.DefaultEnabled != nil || r.MinBudget != nil || r.MaxBudget != nil || r.DynamicBudget != nil || r.ZeroBudget != nil ||
		r.ForcedToolChoice != nil {
		s.Reasoning = &r
	}
	if c.StructuredOutput != types.StructuredOutputNone {
		s.StructuredOutput = ptr(c.StructuredOutput)
	}
	for mt, ok := range c.Media.NativeTypes {
		if ok {
			s.Media = append(s.Media, mt)
		}
	}
	slices.Sort(s.Media)
	s.ServerTools = append([]types.ServerToolKind(nil), c.ServerTools...)
	if len(e.ServerToolFees) > 0 {
		s.ServerToolFees = map[types.ServerToolKind]Fee{}
		for k, f := range e.ServerToolFees {
			s.ServerToolFees[k] = f
		}
	}
	s.Pricing = pricingSpec(c.Pricing)
	s.Defaults = e.Defaults.clone()
	s.Dials = e.Dials.clone()
	s.Notes = append([]string(nil), c.Notes...)
	return s
}

func ptr[T any](v T) *T { return &v }
