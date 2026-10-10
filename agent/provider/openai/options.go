package openai

import "github.com/urmzd/saige/agent/types"

var _ types.OptionsProvider = (*Adapter)(nil)

// withRequestOptions returns a copy of the adapter with opts applied.
func (a *Adapter) withRequestOptions(o types.RequestOptions) (*Adapter, error) {
	caps := a.Capabilities()
	if err := caps.ValidateOptions(o); err != nil {
		return nil, err
	}
	switch {
	case o.TopK != nil:
		return nil, caps.OptionError("top_k", "not supported by the OpenAI chat API")
	case o.ReasoningEnabled != nil:
		return nil, caps.OptionError("reasoning_enabled", "use reasoning_effort with this adapter")
	case o.ReasoningBudget != nil:
		return nil, caps.OptionError("reasoning_budget", "use reasoning_effort with this adapter")
	}
	c := *a
	p := &c.params
	if o.Temperature != nil {
		p.temperature = o.Temperature
	}
	if o.TopP != nil {
		p.topP = o.TopP
	}
	if o.Seed != nil {
		p.seed = o.Seed
	}
	if o.MaxOutputTokens != nil {
		p.maxTokens = o.MaxOutputTokens
	}
	if o.FrequencyPenalty != nil {
		p.frequencyPenalty = o.FrequencyPenalty
	}
	if o.PresencePenalty != nil {
		p.presencePenalty = o.PresencePenalty
	}
	if len(o.StopSequences) > 0 {
		p.stop = o.StopSequences
	}
	if o.ParallelTools != nil {
		p.parallelTools = o.ParallelTools
	}
	if o.ReasoningEffort != nil {
		p.reasoningEffort = o.ReasoningEffort
	}
	if o.ToolChoice != nil {
		p.toolChoice = o.ToolChoice
	}
	return &c, nil
}
