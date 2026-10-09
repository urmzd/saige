package types

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// ErrInvalidModelConfig identifies a request rejected locally, before network I/O.
// These errors are permanent: retrying the same configuration cannot fix them.
var ErrInvalidModelConfig = errors.New("invalid model configuration")

// ErrSchemaUnsupported identifies a request with a response schema sent to a
// provider that cannot enforce one. ErrOptionsUnsupported identifies request
// options sent to a provider that cannot receive them. Both match
// ErrInvalidModelConfig. A decorator that cannot pass the request on returns
// one of them, so a fallback chain can skip that member and try the next.
var (
	ErrSchemaUnsupported  error = &invalidConfigError{msg: "structured_output: provider cannot enforce a response schema"}
	ErrOptionsUnsupported error = &invalidConfigError{msg: "provider does not accept request options"}
)

// invalidConfigError is a sentinel that also matches ErrInvalidModelConfig.
type invalidConfigError struct{ msg string }

func (e *invalidConfigError) Error() string {
	return ErrInvalidModelConfig.Error() + ": " + e.msg
}

func (e *invalidConfigError) Is(target error) bool { return target == ErrInvalidModelConfig }

const reasoningEffortNone = "none"

// RequestOptions describes explicitly requested controls, independently of SDK
// wire types. Nil means omitted; zero and false remain explicit values.
type RequestOptions struct {
	Temperature, TopP, TopK           *float64
	FrequencyPenalty, PresencePenalty *float64
	Seed, MaxOutputTokens             *int64
	StopSequences                     []string
	ParallelTools, ReasoningEnabled   *bool
	ReasoningEffort                   *string
	ReasoningBudget                   *int64
	// ToolChoice constrains whether and which tool the model calls. Nil leaves
	// the provider default (auto).
	ToolChoice *ToolChoice
}

// ToolChoiceMode says how the model may use the offered tools.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"     // the model decides; the zero value means the same
	ToolChoiceNone     ToolChoiceMode = "none"     // the model must not call a tool
	ToolChoiceRequired ToolChoiceMode = "required" // the model must call some tool
	ToolChoiceNamed    ToolChoiceMode = "named"    // the model must call the tool in Name
)

// ToolChoice is a provider-neutral tool_choice control. Name is set only for
// ToolChoiceNamed.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// Validate checks the choice against the tools offered in the request.
func (c ToolChoice) Validate(tools []ToolDef) error {
	switch c.Mode {
	case "", ToolChoiceAuto, ToolChoiceNone:
		if c.Name != "" {
			return fmt.Errorf("%w: tool_choice: name is only valid with mode %q", ErrInvalidModelConfig, ToolChoiceNamed)
		}
	case ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("%w: tool_choice: name is only valid with mode %q", ErrInvalidModelConfig, ToolChoiceNamed)
		}
		if tools != nil && len(tools) == 0 {
			return fmt.Errorf("%w: tool_choice: mode %q needs at least one tool", ErrInvalidModelConfig, c.Mode)
		}
	case ToolChoiceNamed:
		if c.Name == "" {
			return fmt.Errorf("%w: tool_choice: mode %q needs a name", ErrInvalidModelConfig, c.Mode)
		}
		if tools != nil && !slices.ContainsFunc(tools, func(t ToolDef) bool { return t.Name == c.Name }) {
			return fmt.Errorf("%w: tool_choice: tool %q is not offered", ErrInvalidModelConfig, c.Name)
		}
	default:
		return fmt.Errorf("%w: tool_choice: unknown mode %q", ErrInvalidModelConfig, c.Mode)
	}
	return nil
}

// ValidateToolChoice checks a tool choice against the model's declaration and
// the offered tools. Explicit auto matches the provider default and needs no
// capability; every other mode requires CapToolChoice. Pass nil tools to skip
// the tool-list checks.
func (mc ModelCapabilities) ValidateToolChoice(c *ToolChoice, tools []ToolDef) error {
	if c == nil {
		return nil
	}
	if err := c.Validate(tools); err != nil {
		return &ProviderError{Provider: mc.Provider, Model: mc.Model, Kind: ErrorKindPermanent, Err: err}
	}
	if c.Mode == "" || c.Mode == ToolChoiceAuto {
		return nil
	}
	return mc.Require(CapToolChoice)
}

// OptionError names the rejected control without including prompts or secrets.
func (mc ModelCapabilities) OptionError(option, reason string) error {
	return &ProviderError{Provider: mc.Provider, Model: mc.Model,
		Kind: ErrorKindPermanent, Err: fmt.Errorf("%w: %s: %s", ErrInvalidModelConfig, option, reason)}
}

// Require rejects capabilities absent from the declaration. Unknown models use
// their provider baseline; Known=false is never a claim of verified support.
func (mc ModelCapabilities) Require(want ...Capability) error {
	for _, c := range want {
		if !mc.Supports(c) {
			return mc.OptionError(string(c), "not declared supported for this model")
		}
	}
	return nil
}

// ValidateRequest checks the capabilities required by the request shape, in
// addition to ValidateOptions. A streaming chat entry point must not send an
// embedding-only model, tools or a schema that the declaration does not support.
// Unknown models retain conservative/inferred metadata; applications requiring
// exact declarations must also check Known before admission.
func (mc ModelCapabilities) ValidateRequest(tools []ToolDef, schema bool) error {
	if err := mc.Require(CapStreaming); err != nil {
		return err
	}
	if len(tools) > 0 {
		if err := mc.Require(CapTools); err != nil {
			return err
		}
	}
	if schema {
		return mc.Require(CapStructuredOutput)
	}
	return nil
}

// ValidateOptions checks declared support and reasoning interactions without
// changing the caller's settings. It does not attempt to infer capabilities
// from the name or to probe a remote endpoint.
func (mc ModelCapabilities) ValidateOptions(o RequestOptions) error {
	if err := mc.validateOptionSupport(o); err != nil {
		return err
	}
	if err := mc.validateOptionRanges(o); err != nil {
		return err
	}
	if err := mc.ValidateToolChoice(o.ToolChoice, nil); err != nil {
		return err
	}
	return mc.validateReasoningOptions(o)
}

func (mc ModelCapabilities) validateOptionSupport(o RequestOptions) error {
	for _, item := range []struct {
		cap Capability
		set bool
	}{
		{CapTemperature, o.Temperature != nil}, {CapTopP, o.TopP != nil},
		{CapTopK, o.TopK != nil}, {CapSeed, o.Seed != nil},
		{CapMaxOutputTokens, o.MaxOutputTokens != nil},
		{CapFrequencyPenalty, o.FrequencyPenalty != nil}, {CapPresencePenalty, o.PresencePenalty != nil},
		{CapStopSequences, len(o.StopSequences) > 0}, {CapParallelToolControl, o.ParallelTools != nil},
		{CapReasoningEffort, o.ReasoningEffort != nil}, {CapReasoningBudget, o.ReasoningBudget != nil},
		{CapReasoningToggle, o.ReasoningEnabled != nil},
	} {
		if item.set {
			if err := mc.Require(item.cap); err != nil {
				return err
			}
		}
	}
	return nil
}

func (mc ModelCapabilities) validateOptionRanges(o RequestOptions) error {
	for _, item := range []struct {
		name     string
		value    *float64
		min, max float64
	}{
		{"temperature", o.Temperature, 0, 2}, {"top_p", o.TopP, 0, 1},
		{"top_k", o.TopK, 0, math.MaxFloat64},
		{"frequency_penalty", o.FrequencyPenalty, -2, 2}, {"presence_penalty", o.PresencePenalty, -2, 2},
	} {
		if item.value != nil && (math.IsNaN(*item.value) || math.IsInf(*item.value, 0) || *item.value < item.min || *item.value > item.max) {
			return mc.OptionError(item.name, fmt.Sprintf("must be finite and in [%g, %g]", item.min, item.max))
		}
	}
	if o.MaxOutputTokens != nil && (*o.MaxOutputTokens <= 0 || (mc.MaxOutputTokens > 0 && *o.MaxOutputTokens > int64(mc.MaxOutputTokens))) {
		return mc.OptionError("max_output_tokens", "must be positive and within the declared model limit")
	}
	return nil
}

func (mc ModelCapabilities) validateReasoningOptions(o RequestOptions) error {
	modes := 0
	if o.ReasoningEnabled != nil {
		modes++
	}
	if o.ReasoningBudget != nil {
		modes++
	}
	if o.ReasoningEffort != nil {
		modes++
	}
	if modes > 1 {
		return mc.OptionError("reasoning", "use only one of toggle, budget or effort")
	}
	if o.ReasoningEffort != nil {
		if *o.ReasoningEffort == "" || !slices.Contains(mc.ReasoningEfforts, *o.ReasoningEffort) {
			return mc.OptionError("reasoning_effort", fmt.Sprintf("must be one of %v", mc.ReasoningEfforts))
		}
		if mc.ReasoningRequired && *o.ReasoningEffort == reasoningEffortNone {
			return mc.OptionError("reasoning_effort", "reasoning cannot be disabled")
		}
	}
	if o.ReasoningEnabled != nil && !*o.ReasoningEnabled && mc.ReasoningRequired {
		return mc.OptionError("reasoning_enabled", "reasoning cannot be disabled")
	}
	if o.ReasoningBudget != nil {
		n := *o.ReasoningBudget
		switch {
		case n == -1 && mc.DynamicReasoningBudget:
		case n == 0 && mc.ZeroReasoningBudget && !mc.ReasoningRequired:
		case n <= 0 || n < int64(mc.MinReasoningBudget) || (mc.MaxReasoningBudget > 0 && n > int64(mc.MaxReasoningBudget)):
			return mc.OptionError("reasoning_budget", "outside the declared range or disables required reasoning")
		}
	}
	if mc.ReasoningActive(o) {
		for _, item := range []struct {
			cap Capability
			set bool
		}{
			{CapTemperature, o.Temperature != nil}, {CapTopP, o.TopP != nil}, {CapTopK, o.TopK != nil},
		} {
			if item.set && slices.Contains(mc.SamplingRequiresNoReasoning, item.cap) {
				return mc.OptionError(string(item.cap), "requires reasoning to be disabled")
			}
		}
	}
	return nil
}

// ReasoningActive resolves an explicit selection or the cataloged default. It
// does not promise visible thinking text: several APIs expose only final text.
func (mc ModelCapabilities) ReasoningActive(o RequestOptions) bool {
	if o.ReasoningEnabled != nil {
		return *o.ReasoningEnabled
	}
	if o.ReasoningBudget != nil {
		return *o.ReasoningBudget != 0
	}
	if o.ReasoningEffort != nil {
		return *o.ReasoningEffort != reasoningEffortNone
	}
	return mc.ReasoningRequired || mc.ReasoningDefaultEnabled || (mc.DefaultReasoningEffort != "" && mc.DefaultReasoningEffort != reasoningEffortNone)
}

// Option names used by OptionNames, Without and origin maps. They match the
// keys of the catalog's options object. One name covers the three reasoning
// controls, because a request carries at most one of them.
const (
	OptionTemperature      = "temperature"
	OptionTopP             = "top_p"
	OptionTopK             = "top_k"
	OptionFrequencyPenalty = "frequency_penalty"
	OptionPresencePenalty  = "presence_penalty"
	OptionSeed             = "seed"
	OptionMaxOutputTokens  = "max_output_tokens"
	OptionStop             = "stop"
	OptionParallelTools    = "parallel_tools"
	OptionReasoning        = "reasoning"
	OptionToolChoice       = "tool_choice"
)

// AllOptionNames lists every option name in canonical order.
func AllOptionNames() []string {
	return []string{
		OptionTemperature, OptionTopP, OptionTopK, OptionFrequencyPenalty, OptionPresencePenalty,
		OptionSeed, OptionMaxOutputTokens, OptionStop, OptionParallelTools, OptionReasoning, OptionToolChoice,
	}
}

// HasReasoning reports whether any of the three reasoning controls is set.
func (o RequestOptions) HasReasoning() bool {
	return o.ReasoningEnabled != nil || o.ReasoningEffort != nil || o.ReasoningBudget != nil
}

// OptionNames returns the names of the options set in o, in canonical order.
func (o RequestOptions) OptionNames() []string {
	set := map[string]bool{
		OptionTemperature: o.Temperature != nil, OptionTopP: o.TopP != nil, OptionTopK: o.TopK != nil,
		OptionFrequencyPenalty: o.FrequencyPenalty != nil, OptionPresencePenalty: o.PresencePenalty != nil,
		OptionSeed: o.Seed != nil, OptionMaxOutputTokens: o.MaxOutputTokens != nil,
		OptionStop: len(o.StopSequences) > 0, OptionParallelTools: o.ParallelTools != nil,
		OptionReasoning: o.HasReasoning(), OptionToolChoice: o.ToolChoice != nil,
	}
	var out []string
	for _, name := range AllOptionNames() {
		if set[name] {
			out = append(out, name)
		}
	}
	return out
}

// Merge returns o with every option set in over replacing o's value, field by
// field. The reasoning controls are one option: when over sets any of them,
// over's reasoning replaces o's whole, matching the adapters' rule of one
// reasoning control per request. Neither input is modified.
func (o RequestOptions) Merge(over RequestOptions) RequestOptions {
	out := o.Clone()
	over = over.Clone()
	if over.Temperature != nil {
		out.Temperature = over.Temperature
	}
	if over.TopP != nil {
		out.TopP = over.TopP
	}
	if over.TopK != nil {
		out.TopK = over.TopK
	}
	if over.FrequencyPenalty != nil {
		out.FrequencyPenalty = over.FrequencyPenalty
	}
	if over.PresencePenalty != nil {
		out.PresencePenalty = over.PresencePenalty
	}
	if over.Seed != nil {
		out.Seed = over.Seed
	}
	if over.MaxOutputTokens != nil {
		out.MaxOutputTokens = over.MaxOutputTokens
	}
	if len(over.StopSequences) > 0 {
		out.StopSequences = over.StopSequences
	}
	if over.ParallelTools != nil {
		out.ParallelTools = over.ParallelTools
	}
	if over.HasReasoning() {
		out.ReasoningEnabled, out.ReasoningEffort, out.ReasoningBudget = over.ReasoningEnabled, over.ReasoningEffort, over.ReasoningBudget
	}
	if over.ToolChoice != nil {
		out.ToolChoice = over.ToolChoice
	}
	return out
}

// Without returns a copy of o with the named options cleared. Unknown names
// are ignored; "reasoning" clears all three reasoning controls.
func (o RequestOptions) Without(names ...string) RequestOptions {
	out := o.Clone()
	for _, name := range names {
		switch name {
		case OptionTemperature:
			out.Temperature = nil
		case OptionTopP:
			out.TopP = nil
		case OptionTopK:
			out.TopK = nil
		case OptionFrequencyPenalty:
			out.FrequencyPenalty = nil
		case OptionPresencePenalty:
			out.PresencePenalty = nil
		case OptionSeed:
			out.Seed = nil
		case OptionMaxOutputTokens:
			out.MaxOutputTokens = nil
		case OptionStop:
			out.StopSequences = nil
		case OptionParallelTools:
			out.ParallelTools = nil
		case OptionReasoning:
			out.ReasoningEnabled, out.ReasoningEffort, out.ReasoningBudget = nil, nil, nil
		case OptionToolChoice:
			out.ToolChoice = nil
		}
	}
	return out
}

// Clone returns a deep copy, so the result shares no pointers with o.
func (o RequestOptions) Clone() RequestOptions {
	out := RequestOptions{
		Temperature: clonePtr(o.Temperature), TopP: clonePtr(o.TopP), TopK: clonePtr(o.TopK),
		FrequencyPenalty: clonePtr(o.FrequencyPenalty), PresencePenalty: clonePtr(o.PresencePenalty),
		Seed: clonePtr(o.Seed), MaxOutputTokens: clonePtr(o.MaxOutputTokens),
		ParallelTools: clonePtr(o.ParallelTools), ReasoningEnabled: clonePtr(o.ReasoningEnabled),
		ReasoningEffort: clonePtr(o.ReasoningEffort), ReasoningBudget: clonePtr(o.ReasoningBudget),
		ToolChoice: clonePtr(o.ToolChoice),
	}
	if o.StopSequences != nil {
		out.StopSequences = append([]string(nil), o.StopSequences...)
	}
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
