package openai

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

var _ types.OptionsProvider = (*Adapter)(nil)

// ChatStreamWithOptions implements types.OptionsProvider. Each option set in
// opts overrides the adapter's configured value for this call only; unset
// options keep the configured ones. The tool choice maps to OpenAI's
// tool_choice: auto, none, required, or a named function. Options the model
// does not declare, and options this adapter cannot send (top_k, a reasoning
// toggle or budget), fail before any network I/O with an error matching
// types.ErrInvalidModelConfig.
func (a *Adapter) ChatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts types.RequestOptions) (<-chan types.Delta, error) {
	c, err := a.withRequestOptions(opts.Raw())
	if err != nil {
		return nil, err
	}
	if c, err = c.compileDials(opts, tools, false, types.SurfaceChat); err != nil {
		return nil, err
	}
	return c.chatStream(ctx, messages, tools, nil)
}

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
