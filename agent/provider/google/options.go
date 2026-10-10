package google

import (
	"math"
	"strings"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

var _ types.OptionsProvider = (*Adapter)(nil)

// withRequestOptions returns a copy of the adapter with opts applied.
func (a *Adapter) withRequestOptions(o types.RequestOptions) (*Adapter, error) {
	caps := a.Capabilities()
	if err := caps.ValidateOptions(o); err != nil {
		return nil, err
	}
	switch {
	case o.ParallelTools != nil:
		return nil, caps.OptionError("parallel_tools", "not supported by the Gemini API")
	case o.ReasoningEnabled != nil:
		return nil, caps.OptionError("reasoning_enabled", "use a reasoning budget or effort with this adapter")
	}
	toFloat32 := func(p *float64) *float32 {
		v := float32(*p)
		return &v
	}
	c := *a
	g := &c.generation
	if o.Temperature != nil {
		g.Temperature = toFloat32(o.Temperature)
	}
	if o.TopP != nil {
		g.TopP = toFloat32(o.TopP)
	}
	if o.TopK != nil {
		g.TopK = toFloat32(o.TopK)
	}
	if o.FrequencyPenalty != nil {
		g.FrequencyPenalty = toFloat32(o.FrequencyPenalty)
	}
	if o.PresencePenalty != nil {
		g.PresencePenalty = toFloat32(o.PresencePenalty)
	}
	if o.Seed != nil {
		if *o.Seed < math.MinInt32 || *o.Seed > math.MaxInt32 {
			return nil, caps.OptionError("seed", "must fit in 32 bits")
		}
		s := int32(*o.Seed) //nolint:gosec // range checked above
		g.Seed = &s
	}
	if o.MaxOutputTokens != nil {
		if *o.MaxOutputTokens > math.MaxInt32 {
			return nil, caps.OptionError("max_output_tokens", "must fit in 32 bits")
		}
		g.MaxOutputTokens = int32(*o.MaxOutputTokens) //nolint:gosec // range checked above
	}
	if len(o.StopSequences) > 0 {
		g.StopSequences = o.StopSequences
	}
	// One reasoning control per request: a per-call budget or level
	// replaces the configured thinking configuration.
	if o.ReasoningBudget != nil {
		if *o.ReasoningBudget < 0 || *o.ReasoningBudget > math.MaxInt32 {
			return nil, caps.OptionError("reasoning_budget", "must be non-negative and fit in 32 bits")
		}
		b := int32(*o.ReasoningBudget) //nolint:gosec // range checked above
		c.thinking = &genai.ThinkingConfig{ThinkingBudget: &b, IncludeThoughts: b != 0}
	}
	if o.ReasoningEffort != nil {
		c.thinking = &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevel(strings.ToUpper(*o.ReasoningEffort)), IncludeThoughts: true}
	}
	if o.ToolChoice != nil {
		c.toolChoice = o.ToolChoice
	}
	return &c, nil
}
