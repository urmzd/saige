package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

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
	if p.Compaction != nil {
		checkCompaction(path+".compaction", *p.Compaction, found)
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
		if e.Provider == "" && e.Offering == "" && e.Endpoint == "" {
			found.errorf(ep+".provider", CodeMissing, "provider is required (or name an offering or an endpoint)")
		}
		if strings.TrimSpace(string(e.Model)) == "" && e.Offering == "" {
			found.errorf(ep+".model", CodeMissing, "model is required (or name an offering)")
		}
		if e.Options != nil {
			checkOptionsShape(ep+".options", e.Options, found)
		}
		if e.Dials != nil {
			checkDialValues(ep+".dials", *e.Dials, found)
		}
		switch e.Inherit {
		case "", "all", inheritNone:
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

// checkCompaction validates a compaction spec and the steps of a chain.
func checkCompaction(path string, s CompactionSpec, found *issues) {
	switch types.CompactStrategy(s.Strategy) {
	case types.CompactNone, types.CompactSlidingWindow, types.CompactSummarize, types.CompactClearToolResults,
		types.CompactKeepRecent, types.CompactSummary, types.CompactRelevantPlusSummary:
		if len(s.Chain) > 0 {
			found.errorf(path+".chain", CodeBadValue, "chain is only valid with strategy chain")
		}
	case types.CompactChain:
		if len(s.Chain) == 0 {
			found.errorf(path+".chain", CodeBadValue, "strategy chain needs at least one step")
		}
	default:
		found.errorf(path+".strategy", CodeBadValue,
			"strategy must be none, sliding_window, summarize, clear_tool_results, keep_recent, summary, relevant_plus_summary or chain")
	}
	counts := []struct {
		name string
		n    int
	}{
		{"max_input_tokens", s.MaxInputTokens}, {"target_tokens", s.TargetTokens}, {"keep_turns", s.KeepTurns},
		{"select_k", s.SelectK}, {"threshold", s.Threshold}, {"keep_last", s.KeepLast},
		{"window_size", s.WindowSize}, {"keep_tool_results", s.KeepToolResults},
	}
	for _, c := range counts {
		if c.n < 0 {
			found.errorf(path+"."+c.name, CodeBadValue, "%s must not be negative", c.name)
		}
	}
	for i, step := range s.Chain {
		checkCompaction(fmt.Sprintf("%s.chain[%d]", path, i), step, found)
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

func sortedKeys[K ~string, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
