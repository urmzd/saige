package openai

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

var _ catalog.ModelLister = (*Adapter)(nil)

// reasoningNone is the reasoning effort that turns reasoning off.
const reasoningNone = "none"

// WithToolChoice constrains whether and which tool the model calls: auto,
// none, required, or a named function. Modes other than auto require
// CapToolChoice. The choice applies to requests that offer tools; a request
// without tools sends none, because the API rejects tool_choice without tools.
func WithToolChoice(c types.ToolChoice) Option {
	return func(cfg *config) { cfg.params.toolChoice = &c }
}

// checkToolChoice validates the configured choice against the offered tools.
func (a *Adapter) checkToolChoice(tools []types.ToolDef) error {
	if a.params.toolChoice == nil || len(tools) == 0 {
		return nil
	}
	return a.Capabilities().ValidateToolChoice(a.params.toolChoice, tools)
}

// checkChatTools applies the model's Chat Completions tool rule to a request
// that offers tools. A model that accepts tools there only with reasoning
// effort "none" gets that effort when none is configured, so effortNone is
// true; an explicit other effort fails locally, as the API would reject it.
// A model whose tools need the Responses API fails locally; build it with
// NewResponsesAdapter (provider.Build does so).
func (a *Adapter) checkChatTools(tools []types.ToolDef) (effortNone bool, err error) {
	if len(tools) == 0 {
		return false, nil
	}
	caps := a.Capabilities()
	switch caps.ChatCompletionsTools {
	case types.ChatToolsResponsesOnly:
		return false, caps.OptionError("tools", "this model calls tools only through the Responses API; use NewResponsesAdapter")
	case types.ChatToolsNoReasoning:
		if e := a.params.reasoningEffort; e != nil && *e != reasoningNone {
			return false, caps.OptionError("reasoning_effort",
				fmt.Sprintf("%q with tools: Chat Completions accepts tools on this model only with effort \"none\"; use NewResponsesAdapter to keep reasoning", *e))
		}
		return a.params.reasoningEffort == nil, nil
	}
	return false, nil
}

// applyToolChoice encodes the configured choice. The caller has checked it.
func (a *Adapter) applyToolChoice(p *openai.ChatCompletionNewParams) {
	c := a.params.toolChoice
	if c == nil {
		return
	}
	switch c.Mode {
	case types.ToolChoiceNone:
		p.ToolChoice.OfAuto = openai.String("none")
	case types.ToolChoiceRequired:
		p.ToolChoice.OfAuto = openai.String("required")
	case types.ToolChoiceNamed:
		p.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: c.Name})
	default:
		p.ToolChoice.OfAuto = openai.String("auto")
	}
}

// ListModels implements catalog.ModelLister with the Models API. The endpoint
// reports IDs and creation times only; limits come from the catalog. An
// OpenAI-compatible server reached through WithBaseURL lists its own models.
func (a *Adapter) ListModels(ctx context.Context) ([]catalog.RemoteModel, error) {
	pager := a.client.Models.ListAutoPaging(ctx)
	var out []catalog.RemoteModel
	for pager.Next() {
		m := pager.Current()
		rm := catalog.RemoteModel{ID: m.ID}
		if m.Created > 0 {
			rm.Created = time.Unix(m.Created, 0).UTC()
		}
		out = append(out, rm)
	}
	if err := pager.Err(); err != nil {
		return nil, classifyOpenAIError(string(a.model), err, true)
	}
	return out, nil
}
