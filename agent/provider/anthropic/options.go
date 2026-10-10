package anthropic

import (
	"math"

	"github.com/urmzd/saige/agent/types"
)

var _ types.OptionsProvider = (*Adapter)(nil)

// withRequestOptions returns a copy of the adapter with opts applied.
func (a *Adapter) withRequestOptions(o types.RequestOptions) (*Adapter, error) {
	caps := a.Capabilities()
	if err := caps.ValidateOptions(o); err != nil {
		return nil, err
	}
	c := *a
	if o.Temperature != nil {
		c.temperature = o.Temperature
	}
	if o.TopP != nil {
		c.topP = o.TopP
	}
	if o.TopK != nil {
		if *o.TopK != math.Trunc(*o.TopK) {
			return nil, caps.OptionError("top_k", "must be a whole number")
		}
		k := int64(*o.TopK)
		c.topK = &k
	}
	if o.MaxOutputTokens != nil {
		c.maxTokens = *o.MaxOutputTokens
	}
	if len(o.StopSequences) > 0 {
		c.stop = o.StopSequences
	}
	if o.ParallelTools != nil {
		c.parallelTools = o.ParallelTools
	}
	// One reasoning control per request: a per-call budget or effort
	// replaces the configured one.
	if o.ReasoningBudget != nil {
		c.thinking, c.reasoningEffort = o.ReasoningBudget, nil
	}
	if o.ReasoningEffort != nil {
		c.thinking, c.reasoningEffort = nil, o.ReasoningEffort
	}
	if o.ToolChoice != nil {
		c.toolChoice = o.ToolChoice
	}
	return &c, nil
}
