package openai

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/internal/generate"
	"github.com/urmzd/saige/agent/types"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*ResponsesAdapter)(nil)
	_ types.NamedProvider            = (*ResponsesAdapter)(nil)
	_ types.ModelProvider            = (*ResponsesAdapter)(nil)
	_ types.TargetSwitcher           = (*ResponsesAdapter)(nil)
	_ types.CapabilityReporter       = (*ResponsesAdapter)(nil)
	_ types.OptionsProvider          = (*ResponsesAdapter)(nil)
	_ catalog.ModelLister            = (*ResponsesAdapter)(nil)
)

// ResponsesAdapter is an OpenAI provider that uses the Responses API instead
// of Chat Completions. It accepts the same options as NewAdapter and resolves
// the same catalog rows, so the two adapters are interchangeable for a model
// that both APIs serve.
//
// Requests are stateless: each one carries the full conversation and sets
// store to false, so no response is kept on the server and the conversation
// tree stays the only record. The Responses API has no seed, stop sequences,
// or frequency and presence penalties; setting any of them fails before the
// request is sent.
type ResponsesAdapter struct {
	// base holds the client, model and options. Its Chat Completions path is
	// never used.
	base Adapter
}

// NewResponsesAdapter creates an OpenAI provider adapter for the Responses API.
func NewResponsesAdapter(apiKey, model string, opts ...Option) *ResponsesAdapter {
	return &ResponsesAdapter{base: *NewAdapter(apiKey, model, opts...)}
}

// Name implements types.NamedProvider.
func (r *ResponsesAdapter) Name() string { return r.base.Name() }

// Model implements types.ModelProvider.
func (r *ResponsesAdapter) Model() string { return r.base.Model() }

// WithTarget implements types.TargetSwitcher: a model target returns a copy
// of the adapter targeting that model, sharing the underlying client.
func (r *ResponsesAdapter) WithTarget(t types.Target) (types.Provider, error) {
	m, err := types.TargetModel(t, r.Name())
	if err != nil {
		return nil, err
	}
	c := *r
	c.base.model = openai.ChatModel(m)
	return &c, nil
}

// Capabilities implements types.CapabilityReporter.
func (r *ResponsesAdapter) Capabilities() types.ModelCapabilities { return r.base.Capabilities() }

// Offering implements types.OfferingReporter: the model's offering on the
// Responses endpoint, whose input modalities and locators differ from Chat
// Completions'.
func (r *ResponsesAdapter) Offering() types.Offering {
	if o, ok := catalog.LookupOffering(endpointResponses, providerName, string(r.base.model)); ok {
		return o
	}
	if caps := r.Capabilities(); caps.Offering != nil {
		return caps.Offering.Clone()
	}
	return types.OfferingFromCapabilities(r.Capabilities())
}

// endpointResponses names the catalog endpoint of the Responses API.
const endpointResponses = "openai-responses"

func validSummary(mode string) bool {
	switch shared.ReasoningSummary(mode) {
	case shared.ReasoningSummaryAuto, shared.ReasoningSummaryConcise, shared.ReasoningSummaryDetailed:
		return true
	}
	return false
}

// EffectiveOptions implements types.OptionsReporter.
func (r *ResponsesAdapter) EffectiveOptions() types.RequestOptions { return r.base.EffectiveOptions() }

// ListModels implements catalog.ModelLister.
func (r *ResponsesAdapter) ListModels(ctx context.Context) ([]catalog.RemoteModel, error) {
	return r.base.ListModels(ctx)
}

// Validate checks configured controls against the selected model and rejects
// the controls the Responses API does not have.
func (r *ResponsesAdapter) Validate() error {
	if err := r.base.Validate(); err != nil {
		return err
	}
	return r.checkUnsupported(r.base.params)
}

func (r *ResponsesAdapter) checkUnsupported(p genParams) error {
	const reason = "not supported by the OpenAI Responses API"
	caps := r.Capabilities()
	switch {
	case p.seed != nil:
		return caps.OptionError("seed", reason)
	case len(p.stop) > 0:
		return caps.OptionError("stop_sequences", reason)
	case p.frequencyPenalty != nil:
		return caps.OptionError("frequency_penalty", reason)
	case p.presencePenalty != nil:
		return caps.OptionError("presence_penalty", reason)
	case p.audio != nil:
		return caps.OptionError("audio_output", "the OpenAI Responses API has no audio output; use NewAdapter with an audio model")
	case p.reasoningSummary != nil && !validSummary(*p.reasoningSummary):
		return caps.OptionError("reasoning_summary", fmt.Sprintf("%q: use auto, concise or detailed", *p.reasoningSummary))
	}
	return nil
}

// Generate sends a single-turn user prompt with no tools and returns the
// response text.
func (r *ResponsesAdapter) Generate(ctx context.Context, prompt string) (string, error) {
	return generate.Text(ctx, r, prompt)
}

// Stream implements types.Provider. A request may carry a schema, options,
// or both. Every part is mapped natively or the request fails before it is
// sent (see toResponsesInput).
func (r *ResponsesAdapter) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	c, params, err := r.requestParams(req)
	if err != nil {
		return nil, err
	}
	s := c.base.client.Responses.NewStreaming(ctx, params)
	return c.consume(s, req.Schema != nil), nil
}

// SupportsSchema implements types.StructuredOutputProvider.
func (r *ResponsesAdapter) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (r *ResponsesAdapter) SupportsOptions() bool { return true }

// requestParams applies req's options and dials and builds the request. The
// returned adapter carries the request's options.
func (r *ResponsesAdapter) requestParams(req types.Request) (*ResponsesAdapter, responses.ResponseNewParams, error) {
	base, err := r.base.forRequest(req, types.SurfaceResponses)
	if err != nil {
		return nil, responses.ResponseNewParams{}, err
	}
	c := &ResponsesAdapter{base: *base}
	params, err := c.buildParams(req.Messages, req.Tools, req.Schema)
	if err != nil {
		return nil, params, err
	}
	return c, params, nil
}

// buildParams validates the request and encodes it. It sends nothing.
func (r *ResponsesAdapter) buildParams(messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (responses.ResponseNewParams, error) {
	var params responses.ResponseNewParams
	if err := r.Validate(); err != nil {
		return params, err
	}
	caps := r.Capabilities()
	if err := caps.ValidateRequest(tools, schema != nil); err != nil {
		return params, err
	}
	if err := r.base.checkToolChoice(tools); err != nil {
		return params, err
	}
	input, err := toResponsesInput(messages)
	if err != nil {
		return params, wrapPartError(caps, err)
	}

	params.Model = shared.ResponsesModel(r.base.model)
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}
	params.Store = openai.Bool(false)

	p := r.base.params
	if p.maxTokens != nil {
		params.MaxOutputTokens = openai.Int(*p.maxTokens)
	}
	if p.temperature != nil {
		params.Temperature = openai.Float(*p.temperature)
	}
	if p.topP != nil {
		params.TopP = openai.Float(*p.topP)
	}
	if p.reasoningEffort != nil {
		params.Reasoning.Effort = shared.ReasoningEffort(*p.reasoningEffort)
	}
	if p.reasoningSummary != nil {
		params.Reasoning.Summary = shared.ReasoningSummary(*p.reasoningSummary)
	}
	if caps.Supports(types.CapReasoning) {
		// Requests are not stored, so a reasoning item can be sent back in a
		// later turn only with its encrypted content.
		params.Include = []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent}
	}
	if p.parallelTools != nil {
		params.ParallelToolCalls = openai.Bool(*p.parallelTools)
	}
	if err := r.applyPromptCache(&params); err != nil {
		return params, err
	}
	if rTools := toResponsesTools(tools); len(rTools) > 0 {
		params.Tools = rTools
		applyResponsesToolChoice(&params, p.toolChoice)
	}
	if schema != nil {
		schemaMap, strict := responseSchema(*schema)
		params.Text = responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   "response",
					Schema: schemaMap,
					Strict: openai.Bool(strict),
				},
			},
		}
	}
	return params, nil
}

// applyPromptCache encodes WithPromptCache. The Responses API spells the
// retention values the same way as Chat Completions.
func (r *ResponsesAdapter) applyPromptCache(params *responses.ResponseNewParams) error {
	var chat openai.ChatCompletionNewParams
	if err := r.base.applyPromptCache(&chat); err != nil {
		return err
	}
	params.PromptCacheKey = chat.PromptCacheKey
	params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention(chat.PromptCacheRetention)
	return nil
}

func applyResponsesToolChoice(params *responses.ResponseNewParams, c *types.ToolChoice) {
	if c == nil {
		return
	}
	switch c.Mode {
	case types.ToolChoiceNone:
		params.ToolChoice.OfToolChoiceMode = openai.Opt(responses.ToolChoiceOptionsNone)
	case types.ToolChoiceRequired:
		params.ToolChoice.OfToolChoiceMode = openai.Opt(responses.ToolChoiceOptionsRequired)
	case types.ToolChoiceNamed:
		params.ToolChoice.OfFunctionTool = &responses.ToolChoiceFunctionParam{Name: c.Name}
	default:
		params.ToolChoice.OfToolChoiceMode = openai.Opt(responses.ToolChoiceOptionsAuto)
	}
}

// toResponsesTools converts tool definitions to function tools. Strict mode
// is off, matching the Chat Completions adapter: the Responses API turns it
// on by default, and strict mode rejects schemas with optional properties.
func toResponsesTools(defs []types.ToolDef) []responses.ToolUnionParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]responses.ToolUnionParam, len(defs))
	for i, d := range defs {
		out[i] = responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name:        d.Name,
			Description: openai.String(d.Description),
			Parameters:  parameterSchemaToMap(d.Parameters),
			Strict:      openai.Bool(false),
		}}
	}
	return out
}
