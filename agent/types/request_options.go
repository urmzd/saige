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
