package catalog

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Validate checks the catalog as a complete, merged value: every row,
// template and baseline resolves, references and successors resolve, and
// every preset resolves with every chain entry honoring its options. It
// returns a *ValidationError when any issue has error severity; Issues
// returns the warnings too.
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
		found.errorf("version", CodeVersion, "unsupported catalog version %d (this reader supports %d)", c.Version, SchemaVersion)
	}
	for _, name := range sortedKeys(c.Templates) {
		c.checkSpec("templates."+name, c.Templates[name], &found, specTemplate)
		c.chainOf("templates."+name, c.Templates[name], &found, full)
	}
	seen := map[string]int{}
	for i, m := range c.Models {
		path := fmt.Sprintf("models[%d]", i)
		c.checkSpec(path, m, &found, specRow)
		k := rowKey(m.Provider, m.Prefix)
		if j, dup := seen[k]; dup {
			found.errorf(path, CodeDuplicateRow, "row %s is already declared at models[%d]", k, j)
		}
		seen[k] = i
		if !m.Delete && !m.Replace {
			// A layer's row may extend a template a lower layer defines,
			// so its dial values are checked only on the merged catalog.
			if e, ok := c.resolveSpec(path, m, &found, full); ok && full {
				checkRowDials(path, e, &found)
			}
		}
	}
	for _, p := range sortedKeys(c.Baselines) {
		b := c.Baselines[p]
		c.checkSpec("baselines."+p, b, &found, specBaseline)
		c.chainOf("baselines."+p, b, &found, full)
	}
	for _, name := range sortedKeys(c.Presets) {
		c.checkPresetShape("presets."+name, c.Presets[name], &found)
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

type specKind int

const (
	specTemplate specKind = iota
	specRow
	specBaseline
)

// checkSpec validates the names and values one spec declares.
func (c *Catalog) checkSpec(path string, s ModelSpec, found *issues, kind specKind) {
	switch kind {
	case specRow:
		if s.Provider == "" {
			found.errorf(path+".provider", CodeMissing, "provider is required")
		}
		if strings.TrimSpace(s.Prefix) == "" {
			found.errorf(path+".prefix", CodeMissing, "prefix is required")
		}
	case specTemplate, specBaseline:
		if s.Provider != "" || s.Prefix != "" {
			found.errorf(path, CodeBadValue, "provider and prefix belong on model rows, not templates or baselines")
		}
		if s.Replace || s.Delete {
			found.errorf(path, CodeBadValue, "$replace and $delete apply to model rows")
		}
	}
	checkSpecNames(path, s, found)
	checkSpecPricing(path, s, found)
	if s.Defaults != nil {
		checkOptionsShape(path+".defaults", s.Defaults, found)
	}
	checkDialsShape(path+".dials", s.Dials, found)
}

// checkSpecNames validates the capability, media, tier and tool names a spec
// declares.
func checkSpecNames(path string, s ModelSpec, found *issues) {
	known := types.KnownCapabilities()
	for field, list := range map[string][]types.Capability{
		"capabilities": s.Capabilities, "add_capabilities": s.AddCapabilities, "remove_capabilities": s.RemoveCapabilities,
	} {
		for i, cp := range list {
			if !slices.Contains(known, cp) {
				found.errorf(fmt.Sprintf("%s.%s[%d]", path, field, i), CodeUnknownCap, "unknown capability %q%s", cp, hint(string(cp), capNames()))
			}
		}
	}
	if s.Reasoning != nil {
		for i, cp := range s.Reasoning.SamplingRequiresNoReasoning {
			if !slices.Contains(known, cp) {
				found.errorf(fmt.Sprintf("%s.reasoning.sampling_requires_no_reasoning[%d]", path, i), CodeUnknownCap, "unknown capability %q", cp)
			}
		}
		for name, v := range map[string]*int{"min_budget": s.Reasoning.MinBudget, "max_budget": s.Reasoning.MaxBudget} {
			if v != nil && *v < 0 {
				found.errorf(path+".reasoning."+name, CodeBadValue, "must not be negative")
			}
		}
	}
	switch s.ChatCompletionsTools {
	case "", chatToolsAny, string(types.ChatToolsNoReasoning), string(types.ChatToolsResponsesOnly):
	default:
		found.errorf(path+".chat_completions_tools", CodeBadValue, "chat_completions_tools must be any, no_reasoning or responses_only")
	}
	if s.Tier != "" && s.Tier != TierFrontier && s.Tier != TierStandard && s.Tier != TierEconomy {
		found.errorf(path+".tier", CodeBadValue, "tier must be frontier, standard or economy")
	}
	if s.StructuredOutput != nil {
		switch *s.StructuredOutput {
		case types.StructuredOutputNone, types.StructuredOutputNative, types.StructuredOutputToolCall:
		default:
			found.errorf(path+".structured_output", CodeBadValue, "structured_output must be \"\", \"native\" or \"tool_call\"")
		}
	}
	for i, mt := range s.Media {
		if !slices.Contains(types.KnownMediaTypes(), mt) {
			found.errorf(fmt.Sprintf("%s.media[%d]", path, i), CodeUnknownMedia, "unknown media type %q", mt)
		}
	}
	for i, k := range s.ServerTools {
		if !slices.Contains(types.KnownServerToolKinds(), k) {
			found.errorf(fmt.Sprintf("%s.server_tools[%d]", path, i), CodeUnknownTool, "unknown server tool %q", k)
		}
	}
}

// checkSpecPricing validates a spec's fees and rate card.
func checkSpecPricing(path string, s ModelSpec, found *issues) {
	for _, k := range sortedKeys(s.ServerToolFees) {
		f := s.ServerToolFees[k]
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
	if p := s.Pricing; p != nil {
		rates := []float64{p.InputPerMTok, p.OutputPerMTok, p.CachedInputPerMTok, p.CacheWritePerMTok, p.PerRequest}
		for _, r := range rates {
			if r < 0 {
				found.errorf(path+".pricing", CodePricing, "rates must not be negative")
				break
			}
		}
		if (slices.ContainsFunc(rates, func(r float64) bool { return r != 0 }) || p.Free) && p.AsOf == "" {
			found.errorf(path+".pricing.as_of", CodePricing, "as_of is required when a rate is set")
		}
	}
}

func capNames() []string {
	var out []string
	for _, c := range types.KnownCapabilities() {
		out = append(out, string(c))
	}
	return out
}

func hint(s string, candidates []string) string {
	if h := suggest(s, candidates); h != "" {
		return fmt.Sprintf(" (did you mean %q?)", h)
	}
	return ""
}

// checkOptionsShape validates an options object on its own.
func checkOptionsShape(path string, o *OptionsSpec, found *issues) {
	if r := o.Reasoning; r != nil {
		n := 0
		for _, set := range []bool{r.Enabled != nil, r.Effort != nil, r.Budget != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			found.errorf(path+".reasoning", CodeBadValue, "set exactly one of enabled, effort or budget")
		}
	}
	switch o.ToolChoice {
	case "", string(types.ToolChoiceAuto), string(types.ToolChoiceNone):
	default:
		found.errorf(path+".tool_choice", CodeToolChoice,
			"options accept only \"auto\" or \"none\": a forced choice would apply to every turn; set it on the preset's tool_choice, which applies it to one turn")
	}
	if pc := o.PromptCache; pc != nil {
		pp := path + ".prompt_cache"
		switch pc.Mode {
		case PromptCacheOff:
		case PromptCacheMarkers:
			if pc.TTL != "5m" && pc.TTL != "1h" {
				found.errorf(pp+".ttl", CodePromptCache, "markers need a ttl of \"5m\" or \"1h\"")
			}
			if !pc.Tools && !pc.System && !pc.Conversation {
				found.errorf(pp, CodePromptCache, "markers need at least one of tools, system or conversation")
			}
		case PromptCacheAutomatic:
			switch pc.Retention {
			case "", "in_memory", "24h":
			default:
				found.errorf(pp+".retention", CodePromptCache, "retention must be \"\", \"in_memory\" or \"24h\"")
			}
		default:
			found.errorf(pp+".mode", CodePromptCache, "mode must be off, markers or automatic")
		}
		if pc.Mode != PromptCacheMarkers && (pc.TTL != "" || pc.Tools || pc.System || pc.Conversation) {
			found.errorf(pp, CodePromptCache, "ttl, tools, system and conversation apply to mode markers")
		}
		if pc.Mode != PromptCacheAutomatic && (pc.Retention != "" || pc.Key != "") {
			found.errorf(pp, CodePromptCache, "retention and key apply to mode automatic")
		}
	}
	for i, st := range o.ServerTools {
		if err := st.serverTool().Validate(); err != nil {
			found.errorf(fmt.Sprintf("%s.server_tools[%d]", path, i), CodeServerTool, "%v", err)
		}
	}
}

// checkPresetShape validates what a preset declares, before resolution.
func (c *Catalog) checkPresetShape(path string, p PresetSpec, found *issues) {
	if p.Options != nil {
		checkOptionsShape(path+".options", p.Options, found)
		if len(p.Chain) > 1 {
			for _, n := range []string{types.OptionTemperature, types.OptionTopP, types.OptionTopK, types.OptionReasoning} {
				if slices.Contains(p.Options.requestOptions().OptionNames(), n) {
					found.warnf(path+".options."+n, WarnPreferDial,
						"%s is a raw option every entry must accept; prefer the creativity or reasoning dial, which each model maps or drops", n)
				}
			}
		}
	}
	if p.Dials != nil {
		checkDialValues(path+".dials", *p.Dials, found)
	}
	if _, err := parseToolChoice(p.ToolChoice); err != nil {
		found.errorf(path+".tool_choice", CodeToolChoice, "%v", err)
	}
	switch p.OutputMode {
	case "", "auto", "native", "tool", "prompt":
	default:
		found.errorf(path+".output_mode", CodeOutputMode, "output_mode must be auto, native, tool or prompt")
	}
	if p.Routing != nil {
		switch p.Routing.Policy {
		case "", PolicySticky, PolicyAffinity:
		default:
			found.errorf(path+".routing.policy", CodeBadValue, "policy must be sticky or affinity")
		}
		if p.Routing.FailThreshold < 0 {
			found.errorf(path+".routing.fail_threshold", CodeBadValue, "fail_threshold must not be negative")
		}
		if p.Routing.ReprobeAfter < 0 {
			found.errorf(path+".routing.reprobe_after", CodeBadValue, "reprobe_after must not be negative")
		}
		if p.Routing.Policy == PolicySticky && (p.Routing.FailThreshold > 0 || p.Routing.ReprobeAfter > 0) {
			found.errorf(path+".routing.policy", CodeBadValue, "fail_threshold and reprobe_after apply to the affinity policy; remove policy sticky or set affinity")
		}
		for i, cp := range p.Routing.Required {
			if !slices.Contains(types.KnownCapabilities(), cp) {
				found.errorf(fmt.Sprintf("%s.routing.required[%d]", path, i), CodeUnknownCap, "unknown capability %q", cp)
			}
		}
	}
	checkRetry(path+".retry", p.Retry, found)
	for i, e := range p.Chain {
		ep := fmt.Sprintf("%s.chain[%d]", path, i)
		if e.Provider == "" {
			found.errorf(ep+".provider", CodeMissing, "provider is required")
		}
		if strings.TrimSpace(e.Model) == "" {
			found.errorf(ep+".model", CodeMissing, "model is required")
		}
		if e.Options != nil {
			checkOptionsShape(ep+".options", e.Options, found)
		}
		if e.Dials != nil {
			checkDialValues(ep+".dials", *e.Dials, found)
		}
		switch e.Inherit {
		case "", "all", "none":
		default:
			found.errorf(ep+".inherit", CodeBadValue, "inherit must be \"all\" or \"none\"")
		}
		for j, name := range e.Unset {
			if !slices.Contains(unsetNames(), name) {
				found.errorf(fmt.Sprintf("%s.unset[%d]", ep, j), CodeUnset, "unknown option %q%s", name, hint(name, unsetNames()))
			}
		}
		checkRetry(ep+".retry", e.Retry, found)
	}
}

func checkRetry(path string, r *RetrySpec, found *issues) {
	if r == nil {
		return
	}
	if r.MaxAttempts < 0 {
		found.errorf(path+".max_attempts", CodeBadValue, "must not be negative")
	}
	if r.Multiplier < 0 {
		found.errorf(path+".multiplier", CodeBadValue, "must not be negative")
	}
	if r.MaxDelay != 0 && r.BaseDelay > r.MaxDelay {
		found.errorf(path+".base_delay", CodeBadValue, "base_delay exceeds max_delay")
	}
}

// parseToolChoice reads a preset tool choice: auto, none, required or
// named:<tool>.
func parseToolChoice(s string) (*types.ToolChoice, error) {
	switch {
	case s == "":
		return nil, nil
	case s == string(types.ToolChoiceAuto), s == string(types.ToolChoiceNone), s == string(types.ToolChoiceRequired):
		return &types.ToolChoice{Mode: types.ToolChoiceMode(s)}, nil
	case strings.HasPrefix(s, "named:") && len(s) > len("named:"):
		return &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: strings.TrimPrefix(s, "named:")}, nil
	}
	return nil, fmt.Errorf("tool_choice must be auto, none, required or named:<tool>, got %q", s)
}

// checkSuccessors requires every superseded_by to resolve within the
// provider, and forbids cycles.
func (c *Catalog) checkSuccessors(found *issues) {
	v := c.view()
	for i, m := range c.Models {
		if m.Delete {
			continue
		}
		e, ok := v.match(m.Provider, m.Prefix)
		if !ok || e.SupersededBy == "" {
			continue
		}
		path := fmt.Sprintf("models[%d].superseded_by", i)
		seen := map[string]bool{e.Prefix: true}
		next := e.SupersededBy
		for next != "" {
			row, ok := v.match(m.Provider, next)
			if !ok {
				found.errorf(path, CodeUnknownSuccessor, "successor %q matches no %s row", next, m.Provider)
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

func sortedKeys[K ~string, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
