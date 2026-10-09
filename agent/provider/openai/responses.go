package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/internal/generate"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*ResponsesAdapter)(nil)
	_ types.NamedProvider            = (*ResponsesAdapter)(nil)
	_ types.ModelProvider            = (*ResponsesAdapter)(nil)
	_ types.ModelSwitcher            = (*ResponsesAdapter)(nil)
	_ types.CapabilityReporter       = (*ResponsesAdapter)(nil)
	_ types.ContentNegotiator        = (*ResponsesAdapter)(nil)
	_ types.OptionsProvider          = (*ResponsesAdapter)(nil)
	_ catalog.ModelLister            = (*ResponsesAdapter)(nil)
)

// Responses API stream event and item types the adapter reads.
const (
	eventOutputItemAdded   = "response.output_item.added"
	eventOutputItemDone    = "response.output_item.done"
	eventArgumentsDelta    = "response.function_call_arguments.delta"
	eventArgumentsDone     = "response.function_call_arguments.done"
	eventOutputTextDelta   = "response.output_text.delta"
	eventRefusalDelta      = "response.refusal.delta"
	eventCompleted         = "response.completed"
	eventIncomplete        = "response.incomplete"
	eventFailed            = "response.failed"
	eventError             = "error"
	itemFunctionCall       = "function_call"
	finishStop             = "stop"
	finishLength           = "length"
	incompleteOutputTokens = "max_output_tokens"
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

// WithModel implements types.ModelSwitcher: it returns a copy of the adapter
// targeting the given model, sharing the underlying client.
func (r *ResponsesAdapter) WithModel(model string) types.Provider {
	c := *r
	c.base.model = openai.ChatModel(model)
	return &c
}

// Capabilities implements types.CapabilityReporter.
func (r *ResponsesAdapter) Capabilities() types.ModelCapabilities { return r.base.Capabilities() }

// ContentSupport implements types.ContentNegotiator. Audio input is not part
// of the Responses API input format, so audio files are described in text.
func (r *ResponsesAdapter) ContentSupport() types.ContentSupport { return r.base.ContentSupport() }

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
	}
	return nil
}

// Generate sends a single-turn user prompt with no tools and returns the
// response text.
func (r *ResponsesAdapter) Generate(ctx context.Context, prompt string) (string, error) {
	return generate.Text(ctx, r, prompt)
}

// ChatStream implements types.Provider.
func (r *ResponsesAdapter) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return r.stream(ctx, messages, tools, nil)
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
func (r *ResponsesAdapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	return r.stream(ctx, messages, tools, schema)
}

// ChatStreamWithOptions implements types.OptionsProvider. It follows the
// rules of Adapter.ChatStreamWithOptions and also rejects the controls the
// Responses API does not have.
func (r *ResponsesAdapter) ChatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts types.RequestOptions) (<-chan types.Delta, error) {
	base, err := r.base.withRequestOptions(opts)
	if err != nil {
		return nil, err
	}
	c := &ResponsesAdapter{base: *base}
	return c.stream(ctx, messages, tools, nil)
}

// buildParams validates the request and encodes it. It sends nothing.
func (r *ResponsesAdapter) buildParams(messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (responses.ResponseNewParams, error) {
	var params responses.ResponseNewParams
	if err := r.Validate(); err != nil {
		return params, err
	}
	if err := r.Capabilities().ValidateRequest(tools, schema != nil); err != nil {
		return params, err
	}
	if err := r.base.checkToolChoice(tools); err != nil {
		return params, err
	}

	params.Model = shared.ResponsesModel(r.base.model)
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: toResponsesInput(messages)}
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
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(*p.reasoningEffort)}
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

func (r *ResponsesAdapter) stream(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	params, err := r.buildParams(messages, tools, schema)
	if err != nil {
		return nil, err
	}
	s := r.base.client.Responses.NewStreaming(ctx, params)
	return r.consume(s, schema != nil), nil
}

// applyPromptCache encodes WithPromptCache. The Responses API spells the
// in-memory retention "in-memory".
func (r *ResponsesAdapter) applyPromptCache(params *responses.ResponseNewParams) error {
	var chat openai.ChatCompletionNewParams
	if err := r.base.applyPromptCache(&chat); err != nil {
		return err
	}
	params.PromptCacheKey = chat.PromptCacheKey
	switch chat.PromptCacheRetention {
	case "":
	case promptCacheInMemory:
		params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetentionInMemory
	default:
		params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention(chat.PromptCacheRetention)
	}
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

// toResponsesInput converts the conversation to Responses input items. Text
// and files become messages, a tool call becomes a function_call item, and a
// tool result becomes a function_call_output item. Images in a tool result
// follow as a user message, since a function_call_output here carries text.
func toResponsesInput(msgs []types.Message) responses.ResponseInputParam {
	var out responses.ResponseInputParam
	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			var text []string
			var results []types.ToolResultContent
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					text = append(text, bc.Text)
				case types.ToolResultContent:
					results = append(results, bc)
				}
			}
			if len(text) > 0 {
				out = append(out, responses.ResponseInputItemParamOfMessage(strings.Join(text, ""), responses.EasyInputMessageRoleSystem))
			}
			out = appendResponsesToolResults(out, results)

		case types.UserMessage:
			var parts responses.ResponseInputMessageContentListParam
			var results []types.ToolResultContent
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					parts = append(parts, inputText(bc.Text))
				case types.FileContent:
					parts = append(parts, fileContentToInput(bc))
				case types.ToolResultContent:
					results = append(results, bc)
				}
			}
			out = appendResponsesToolResults(out, results)
			if len(parts) > 0 {
				out = append(out, responses.ResponseInputItemParamOfMessage(parts, responses.EasyInputMessageRoleUser))
			}

		case types.AssistantMessage:
			var text strings.Builder
			flush := func() {
				if text.Len() > 0 {
					out = append(out, responses.ResponseInputItemParamOfMessage(text.String(), responses.EasyInputMessageRoleAssistant))
					text.Reset()
				}
			}
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					text.WriteString(bc.Text)
				case types.ToolUseContent:
					flush()
					args, _ := json.Marshal(bc.Arguments)
					out = append(out, responses.ResponseInputItemParamOfFunctionCall(string(args), bc.ID, bc.Name))
				}
			}
			flush()
		}
	}
	return out
}

// appendResponsesToolResults appends a function_call_output per result, then
// one user message per image block.
func appendResponsesToolResults(out responses.ResponseInputParam, results []types.ToolResultContent) responses.ResponseInputParam {
	for _, tr := range results {
		out = append(out, responses.ResponseInputItemParamOfFunctionCallOutput(tr.ToolCallID, openAIToolResultText(tr)))
	}
	for _, tr := range results {
		for _, b := range tr.Blocks {
			if b.Kind == types.ToolResultBlockImage && b.Data != nil && isImageType(b.MediaType) {
				img := inputImage(dataURI(b.MediaType, b.Data))
				out = append(out, responses.ResponseInputItemParamOfMessage(
					responses.ResponseInputMessageContentListParam{img}, responses.EasyInputMessageRoleUser))
			}
		}
	}
	return out
}

func dataURI(mt types.MediaType, data []byte) string {
	return fmt.Sprintf("data:%s;base64,%s", mt, base64.StdEncoding.EncodeToString(data))
}

func inputText(text string) responses.ResponseInputContentUnionParam {
	return responses.ResponseInputContentUnionParam{OfInputText: &responses.ResponseInputTextParam{Text: text}}
}

func inputImage(url string) responses.ResponseInputContentUnionParam {
	return responses.ResponseInputContentUnionParam{OfInputImage: &responses.ResponseInputImageParam{
		ImageURL: openai.String(url),
		Detail:   responses.ResponseInputImageDetailAuto,
	}}
}

// fileContentToInput maps a file to an input part: images and PDFs pass
// through, readable text is inlined, and anything else is described.
func fileContentToInput(fc types.FileContent) responses.ResponseInputContentUnionParam {
	switch {
	case fc.Data != nil && isImageType(fc.MediaType):
		return inputImage(dataURI(fc.MediaType, fc.Data))
	case fc.URI != "" && isImageType(fc.MediaType):
		return inputImage(fc.URI)
	case fc.Data != nil && fc.MediaType == types.MediaPDF:
		name := fc.Filename
		if name == "" {
			name = "document.pdf"
		}
		return responses.ResponseInputContentUnionParam{OfInputFile: &responses.ResponseInputFileParam{
			FileData: openai.String(dataURI(fc.MediaType, fc.Data)),
			Filename: openai.String(name),
		}}
	}
	desc := fmt.Sprintf("[File: %s, type: %s]", fc.Filename, fc.MediaType)
	if fc.Data != nil && isTextType(fc.MediaType) {
		desc += "\n" + string(fc.Data)
	}
	return inputText(desc)
}

// responsesStream is the part of the SDK stream the adapter reads.
type responsesStream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
}

// responsesCall tracks one streamed function call by its output item ID.
type responsesCall struct {
	id, name string
	args     strings.Builder
	ended    bool
}

// responsesState is the translation state of one streamed response.
type responsesState struct {
	out         chan<- types.Delta
	emitted     bool
	textStarted bool
	calls       map[string]*responsesCall
	order       []string
	argsFailure streamcheck.ArgsFailure
	// finish is the normalized finish reason: stop, length, or the
	// incomplete reason the API reported. Empty means the response did not
	// reach a terminal event.
	finish       string
	outputTokens int
	failure      error
}

func (s *responsesState) emit(d types.Delta) {
	if _, usage := d.(types.UsageDelta); !usage {
		s.emitted = true
	}
	s.out <- d
}

func (s *responsesState) endText() {
	if s.textStarted {
		s.emit(types.TextEndDelta{})
		s.textStarted = false
	}
}

// closeCall closes c. complete reports that the model finished writing it.
func (s *responsesState) closeCall(c *responsesCall, complete bool) {
	if c == nil || c.ended {
		return
	}
	c.ended = true
	args, err := streamcheck.DecodeArguments(c.args.String())
	switch {
	case err == nil:
		s.emit(types.ToolCallEndDelta{ID: c.id, Arguments: args})
	case complete:
		s.emit(types.ToolCallEndDelta{ID: c.id, ArgumentsError: err.Error()})
	default:
		s.argsFailure.Set(c.id, c.name, err)
	}
}

func (s *responsesState) closeAll(complete bool) {
	for _, id := range s.order {
		s.closeCall(s.calls[id], complete)
	}
}

func (s *responsesState) text(delta string) {
	if delta == "" {
		return
	}
	if !s.textStarted {
		s.emit(types.TextStartDelta{})
		s.textStarted = true
	}
	s.emit(types.TextContentDelta{Content: delta})
}

// handle translates one stream event.
func (s *responsesState) handle(model string, ev responses.ResponseStreamEventUnion) {
	switch ev.Type {
	case eventOutputTextDelta, eventRefusalDelta:
		s.text(ev.Delta)
	case eventOutputItemAdded:
		if ev.Item.Type != itemFunctionCall {
			return
		}
		s.endText()
		// The model writes items in sequence, so a new call means every
		// earlier one is complete.
		s.closeAll(true)
		c := &responsesCall{id: ev.Item.CallID, name: ev.Item.Name}
		s.calls[ev.Item.ID] = c
		s.order = append(s.order, ev.Item.ID)
		s.emit(types.ToolCallStartDelta{ID: c.id, Name: c.name})
	case eventArgumentsDelta:
		if c := s.calls[ev.ItemID]; c != nil && !c.ended && ev.Delta != "" {
			c.args.WriteString(ev.Delta)
			s.emit(types.ToolCallArgumentDelta{ID: c.id, Content: ev.Delta})
		}
	case eventArgumentsDone:
		if c := s.calls[ev.ItemID]; c != nil && !c.ended {
			if c.args.Len() == 0 && ev.Arguments != "" {
				c.args.WriteString(ev.Arguments)
			}
			s.closeCall(c, true)
		}
	case eventOutputItemDone:
		if ev.Item.Type == itemFunctionCall {
			s.closeCall(s.calls[ev.Item.ID], true)
		}
	case eventCompleted, eventIncomplete:
		s.finish = finishStop
		if ev.Type == eventIncomplete {
			s.finish = ev.Response.IncompleteDetails.Reason
			if s.finish == incompleteOutputTokens || s.finish == "" {
				s.finish = finishLength
			}
		}
		s.endText()
		// A call cut off by the output limit or the safety system is not
		// complete.
		s.closeAll(!types.IsTruncationFinishReason(s.finish) && !types.IsContentFilterFinishReason(s.finish))
		s.usage(ev.Response)
	case eventFailed:
		s.failure = streamcheck.EventError(providerName, model, ev.Response.Error.RawJSON(),
			fmt.Errorf("response failed: %s", ev.Response.Error.Message))
	case eventError:
		s.failure = streamcheck.EventError(providerName, model, ev.RawJSON(),
			fmt.Errorf("stream error: %s", ev.Message))
	}
}

func (s *responsesState) usage(resp responses.Response) {
	u := resp.Usage
	s.outputTokens = int(u.OutputTokens)
	if u.TotalTokens == 0 {
		return
	}
	s.emit(types.UsageDelta{Cumulative: true,
		PromptTokens:       int(u.InputTokens),
		CachedPromptTokens: int(u.InputTokensDetails.CachedTokens),
		CompletionTokens:   int(u.OutputTokens),
		TotalTokens:        int(u.TotalTokens),
		ResponseID:         resp.ID,
		ResponseModel:      string(resp.Model),
		FinishReasons:      []string{s.finish},
	})
}

// consume translates the event stream into deltas. Failure handling matches
// the Chat Completions adapter: a call the model finished writing whose
// arguments do not decode closes with ArgumentsError, a call cut off by the
// output limit is never closed and the turn fails as truncated, and a stream
// that ends without a terminal event fails as incomplete.
func (r *ResponsesAdapter) consume(stream responsesStream, structured bool) <-chan types.Delta {
	model := string(r.base.model)
	maxTokens := 0
	if r.base.params.maxTokens != nil {
		maxTokens = int(*r.base.params.maxTokens)
	}
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)
		s := &responsesState{out: out, calls: map[string]*responsesCall{}}
		for s.failure == nil && stream.Next() {
			s.handle(model, stream.Current())
		}
		err := stream.Err()
		s.endText()
		switch {
		case s.failure != nil:
			out <- types.ErrorDelta{Error: s.failure}
		case err != nil:
			out <- types.ErrorDelta{Error: classifyOpenAIError(model, err, !s.emitted)}
		case s.finish == "":
			out <- types.ErrorDelta{Error: streamcheck.StreamError(providerName, model, streamcheck.ErrIncompleteStream, !s.emitted)}
		case types.IsContentFilterFinishReason(s.finish):
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, model, s.finish)}
		case s.argsFailure.Failed():
			out <- types.ErrorDelta{Error: s.argsFailure.Error(providerName, model, s.finish, s.outputTokens, maxTokens)}
		case structured && types.IsTruncationFinishReason(s.finish):
			out <- types.ErrorDelta{Error: streamcheck.Truncated(providerName, model, s.finish, s.outputTokens, maxTokens)}
		}
	}()
	return out
}
