package anthropic

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/internal/generate"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// blockServerToolUse is the content block type of a server-side tool call.
const blockServerToolUse = "server_tool_use"

// stopPauseTurn is the stop reason of a server tool turn the API paused.
const stopPauseTurn = "pause_turn"

// errPausedTurn reports a server tool turn the API paused before its answer.
var errPausedTurn = errors.New("response paused during server tool use before a final answer; the turn cannot be resumed")

// stopContextWindowExceeded is the stop reason of a response cut off because
// the conversation reached the model's context window.
const stopContextWindowExceeded = string(anthropic.StopReasonModelContextWindowExceeded)

// errContextWindowExceeded reports a response the context window cut off.
var errContextWindowExceeded = errors.New("response stopped at the model's context window before a final answer")

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
	_ types.ModelProvider            = (*Adapter)(nil)
	_ types.ModelSwitcher            = (*Adapter)(nil)
	_ types.CapabilityReporter       = (*Adapter)(nil)
	_ types.ContentNegotiator        = (*Adapter)(nil)
	_ types.OptionsReporter          = (*Adapter)(nil)
)

// Adapter wraps the official Anthropic SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	cachePolicy     PromptCachePolicy
	client          anthropic.Client
	model           anthropic.Model
	maxTokens       int64
	thinking        *int64  // manual thinking budget; nil uses the model default
	reasoningEffort *string // adaptive thinking when set

	temperature   *float64
	topP          *float64
	topK          *int64
	stop          []string
	baseURL       string
	parallelTools *bool
	toolChoice    *types.ToolChoice
	serverTools   []types.ServerTool
	maxRetries    int
	requestOpts   []option.RequestOption
	dials         []types.DialLayer
	dialPolicy    *types.DialPolicy
}

// Option configures the Anthropic adapter.
type Option func(*Adapter)

// WithMaxTokens sets the max tokens for responses. Anthropic requires this on
// every request, which is why the adapter defaults it to 4096 rather than
// leaving it unset.
func WithMaxTokens(n int64) Option {
	return func(a *Adapter) { a.maxTokens = n }
}

// WithThinking enables extended thinking with the given token budget.
// The budget must be at least ModelCapabilities.MinReasoningBudget (1024) and
// the model must declare CapReasoningBudget. Unsupported settings fail locally.
func WithThinking(budgetTokens int64) Option {
	return func(a *Adapter) { a.thinking = &budgetTokens }
}

// WithReasoningEffort enables adaptive thinking and sets its effort. Models
// accepting only a manual budget reject this option; use WithThinking for them.
func WithReasoningEffort(effort string) Option {
	return func(a *Adapter) { a.reasoningEffort = &effort }
}

// WithTemperature sets sampling temperature. Anthropic constrains combining
// this with extended thinking; incompatible settings fail locally.
func WithTemperature(t float64) Option {
	return func(a *Adapter) { a.temperature = &t }
}

// WithTopP sets nucleus sampling. Subject to the same thinking constraint as
// WithTemperature.
func WithTopP(p float64) Option {
	return func(a *Adapter) { a.topP = &p }
}

// WithTopK sets top-k sampling.
func WithTopK(k int64) Option {
	return func(a *Adapter) { a.topK = &k }
}

// WithStopSequences sets sequences that end generation.
func WithStopSequences(stop ...string) Option {
	return func(a *Adapter) { a.stop = stop }
}

// WithParallelToolCalls turns parallel tool use on or off. Anthropic expresses
// "off" as disable_parallel_tool_use on the tool choice, so it rides on the
// choice WithToolChoice configures (auto by default). It is not applied when
// the structured-output path forces its hidden tool, since overriding that
// would break it.
func WithParallelToolCalls(enabled bool) Option {
	return func(a *Adapter) { a.parallelTools = &enabled }
}

// WithBaseURL overrides the API base URL, for gateways and proxies.
func WithBaseURL(url string) Option {
	return func(a *Adapter) { a.baseURL = url }
}

// WithMaxRetries sets how many times the SDK itself retries a failed request.
// The default is 0: retries belong to retry.Provider, which counts every
// attempt, honors Retry-After, and reports RetryError. SDK retries run inside
// one ChatStream call, hidden from that accounting, and stack under an outer
// retry decorator. Set a positive value only for a bare adapter that has no
// retry decorator.
func WithMaxRetries(n int) Option {
	return func(a *Adapter) { a.maxRetries = max(n, 0) }
}

// WithRequestOptions appends SDK request options to every call, for settings
// this package does not wrap (custom headers, an HTTP client, middleware).
// They apply after the adapter's own options, so they can override them.
func WithRequestOptions(opts ...option.RequestOption) Option {
	return func(a *Adapter) { a.requestOpts = append(a.requestOpts, opts...) }
}

// NewAdapter creates a new Anthropic provider adapter using the official SDK.
func NewAdapter(apiKey, model string, opts ...Option) *Adapter {
	a := &Adapter{
		model:     anthropic.Model(model),
		maxTokens: 4096,
	}
	for _, o := range opts {
		o(a)
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if a.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(a.baseURL))
	}
	clientOpts = append(clientOpts, option.WithMaxRetries(a.maxRetries))
	clientOpts = append(clientOpts, a.requestOpts...)
	a.client = anthropic.NewClient(clientOpts...)
	return a
}

// applyParams encodes controls already checked by Validate.
func (a *Adapter) applyParams(p *anthropic.MessageNewParams) {

	thinkingOn := a.thinking != nil
	if a.reasoningEffort != nil {
		p.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
		p.OutputConfig.Effort = anthropic.OutputConfigEffort(*a.reasoningEffort)
	}
	if thinkingOn {
		p.Thinking = anthropic.ThinkingConfigParamOfEnabled(*a.thinking)
	}
	if a.temperature != nil {
		p.Temperature = anthropic.Float(*a.temperature)
	}
	if a.topP != nil {
		p.TopP = anthropic.Float(*a.topP)
	}
	if a.topK != nil {
		p.TopK = anthropic.Int(*a.topK)
	}
	if len(a.stop) > 0 {
		p.StopSequences = a.stop
	}
}

// EffectiveOptions implements types.OptionsReporter: the adapter's configured
// controls expressed as request options, including the max_tokens default
// every request carries.
func (a *Adapter) EffectiveOptions() types.RequestOptions {
	o := a.requestOptions()
	o.DialLayers, o.DialPolicy = a.dials, a.dialPolicy
	return o.Clone()
}

// requestOptions expresses the adapter's configured controls as request
// options, so the shared validation and reasoning rules apply to them.
func (a *Adapter) requestOptions() types.RequestOptions {
	var topK *float64
	if a.topK != nil {
		k := float64(*a.topK)
		topK = &k
	}
	return types.RequestOptions{Temperature: a.temperature, TopP: a.topP, TopK: topK,
		MaxOutputTokens: &a.maxTokens, StopSequences: a.stop, ParallelTools: a.parallelTools,
		ReasoningBudget: a.thinking, ReasoningEffort: a.reasoningEffort, ToolChoice: a.toolChoice}
}

// forcedToolBlocked reports why this adapter cannot send a forced tool
// choice, or "" when it can. A manual thinking budget accepts only auto and
// none, and some models reject forcing outright (RejectsForcedToolChoice).
// Adaptive thinking alone does not block forcing: the API accepts it.
func (a *Adapter) forcedToolBlocked(caps types.ModelCapabilities) string {
	switch {
	case a.thinking != nil:
		return "extended thinking with a manual budget accepts only auto or none"
	case caps.RejectsForcedToolChoice:
		return "this model rejects a forced tool choice (required or named)"
	}
	return ""
}

// Validate rejects unsupported or incompatible controls before any request.
func (a *Adapter) Validate() error {
	caps := a.Capabilities()
	o := a.requestOptions()
	if err := caps.ValidateOptions(o); err != nil {
		return err
	}
	if err := a.validateTools(caps); err != nil {
		return err
	}
	if a.temperature != nil && *a.temperature > 1 {
		return caps.OptionError("temperature", "must be between 0 and 1")
	}
	if a.thinking != nil && *a.thinking >= a.maxTokens {
		return caps.OptionError("reasoning_budget", "must be smaller than max_output_tokens")
	}
	if caps.ReasoningActive(o) {
		if a.temperature != nil && *a.temperature != 1 {
			return caps.OptionError("temperature", "cannot modify temperature while thinking")
		}
		if a.topK != nil {
			return caps.OptionError("top_k", "cannot set top_k while thinking")
		}
		if a.topP != nil && *a.topP < 0.95 {
			return caps.OptionError("top_p", "must be in [0.95, 1] while thinking")
		}
	}
	return nil
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return "anthropic" }

// Model implements types.ModelProvider.
func (a *Adapter) Model() string { return string(a.model) }

// WithModel implements types.ModelSwitcher: it returns a copy of the adapter
// targeting the given model, sharing the underlying client.
func (a *Adapter) WithModel(model string) types.Provider {
	c := *a
	c.model = anthropic.Model(model)
	return &c
}

// Generate sends a single-turn user prompt with no tools and returns the
// response text. It is the simple generation seam used by eval judges, HyDE,
// context compression, and KG extraction.
func (a *Adapter) Generate(ctx context.Context, prompt string) (string, error) {
	return generate.Text(ctx, a, prompt)
}

// ChatStream implements types.Provider.
func (a *Adapter) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	a, err := a.compileDials(types.RequestOptions{}, tools, false)
	if err != nil {
		return nil, err
	}
	if err := a.Capabilities().ValidateRequest(tools, false); err != nil {
		return nil, err
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}
	if err := a.checkToolChoice(tools); err != nil {
		return nil, err
	}

	systemBlocks, aMsgs := toAnthropicParams(messages)
	aTools := append(toAnthropicTools(tools), a.serverToolParams()...)

	params := anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  aMsgs,
		System:    systemBlocks,
	}
	if len(aTools) > 0 {
		params.Tools = aTools
	}
	a.applyParams(&params)
	a.applyToolChoice(&params, len(params.Tools) > 0)
	if err := a.applyPromptCache(&params); err != nil {
		return nil, err
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	return a.consumeStream(stream, nil), nil
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
// This adapter constrains output with a hidden tool and forces the model to call it.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	a, err := a.compileDials(types.RequestOptions{}, tools, schema != nil)
	if err != nil {
		return nil, err
	}
	if schema != nil {
		// Schema output forces a hidden tool, which the API rejects with a
		// manual thinking budget and on models that refuse forcing. This
		// error is returned before any request, so fallback can try another
		// member.
		if why := a.forcedToolBlocked(catalog.MustLookup("anthropic", string(a.model))); why != "" {
			return nil, schemacheck.Unsupported(a, "forced-tool schema output: "+why)
		}
	}
	if err := a.Capabilities().ValidateRequest(tools, schema != nil); err != nil {
		return nil, err
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}

	if schema != nil {
		if err := a.Capabilities().Require(types.CapTools, types.CapToolChoice); err != nil {
			return nil, err
		}
		if err := a.checkSchemaToolChoice(); err != nil {
			return nil, err
		}
	}
	if err := a.checkToolChoice(tools); err != nil {
		return nil, err
	}

	systemBlocks, aMsgs := toAnthropicParams(messages)
	aTools := append(toAnthropicTools(tools), a.serverToolParams()...)

	params := anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  aMsgs,
		System:    systemBlocks,
	}
	a.applyParams(&params)

	if schema != nil {
		// Inject a hidden tool whose input schema is the desired response schema.
		props := make(map[string]any, len(schema.Properties))
		for k, v := range schema.Properties {
			props[k] = propertyToSchema(v)
		}
		hiddenTool := anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        "structured_output",
				Description: anthropic.String("Return the structured response"),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   schema.Required,
				},
			},
		}
		aTools = append(aTools, hiddenTool)
		params.ToolChoice = anthropic.ToolChoiceParamOfTool("structured_output")
	}

	if len(aTools) > 0 {
		params.Tools = aTools
	}
	a.applyToolChoice(&params, len(params.Tools) > 0)
	if err := a.applyPromptCache(&params); err != nil {
		return nil, err
	}

	isStructured := func(name string) bool {
		return schema != nil && name == "structured_output"
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	return a.consumeStream(stream, isStructured), nil
}

// consumeStream reads from the Anthropic streaming response and emits deltas.
// If isStructuredTool is non-nil and returns true for a tool_use block name,
// the tool's input JSON is emitted as text deltas instead of tool call deltas.
//
// A tool call whose argument JSON does not decode is held open, because
// Anthropic sends content_block_stop before the message_delta that carries
// stop_reason. When a later block starts, or the stop reason is not
// max_tokens, the model finished writing the call: it closes with
// ToolCallEndDelta.ArgumentsError set and nil Arguments, so the loop refuses
// it and the model can correct it. A max_tokens stop, or a stream that ends
// before any stop reason, never closes the call and reports a truncation or
// an incomplete stream instead. Malformed structured output is always an
// error. Errors arrive after the usage delta, so the consumer can still
// account for the tokens.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Adapter) consumeStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], isStructuredTool func(string) bool) <-chan types.Delta {
	out := make(chan types.Delta, 64)
	model := string(a.model)
	maxTokens := int(a.maxTokens)
	go func() {
		defer close(out)

		var currentBlockType string
		var currentBlockName string
		var currentBlockID string
		var toolArgsBuf []byte
		var signatureBuf string
		// serverKinds remembers each server tool call's kind, so its result
		// block, which carries only the call ID, reports the same kind.
		serverKinds := map[string]types.ServerToolKind{}

		// Track response metadata for the final UsageDelta.
		var responseID string
		var responseModel string
		var finishReason string
		var outputTokens int
		stopped := false

		// emitted turns true once a content delta reaches the consumer; after
		// that a transport error can no longer be retried.
		emitted := false
		var argsFailure streamcheck.ArgsFailure
		emit := func(d types.Delta) {
			if _, usage := d.(types.UsageDelta); !usage {
				emitted = true
			}
			out <- d
		}
		// heldCall is a tool call whose arguments did not decode, waiting to
		// learn whether the model finished writing it.
		var heldCall *streamcheck.ArgsFailure
		releaseHeld := func(complete bool) {
			if heldCall == nil {
				return
			}
			if complete {
				emit(types.ToolCallEndDelta{ID: heldCall.ToolCallID, ArgumentsError: heldCall.Err.Error()})
			} else {
				argsFailure.Set(heldCall.ToolCallID, heldCall.Name, heldCall.Err)
			}
			heldCall = nil
		}

		for stream.Next() {
			evt := stream.Current()

			switch evt.Type {
			case "message_start":
				responseID = evt.Message.ID
				responseModel = string(evt.Message.Model)
				if evt.Message.Usage.InputTokens+evt.Message.Usage.CacheReadInputTokens+evt.Message.Usage.CacheCreationInputTokens > 0 {
					emit(types.UsageDelta{Cumulative: true,
						CompletionTokens:   int(evt.Message.Usage.OutputTokens),
						PromptTokens:       int(evt.Message.Usage.InputTokens + evt.Message.Usage.CacheReadInputTokens + evt.Message.Usage.CacheCreationInputTokens),
						CachedPromptTokens: int(evt.Message.Usage.CacheReadInputTokens),
						CacheWriteTokens:   int(evt.Message.Usage.CacheCreationInputTokens),
						TotalTokens:        int(evt.Message.Usage.InputTokens + evt.Message.Usage.CacheReadInputTokens + evt.Message.Usage.CacheCreationInputTokens + evt.Message.Usage.OutputTokens),
						ResponseID:         evt.Message.ID,
						ResponseModel:      string(evt.Message.Model),
					})
				}

			case "content_block_start":
				releaseHeld(true)
				currentBlockType = evt.ContentBlock.Type
				currentBlockName = evt.ContentBlock.Name
				currentBlockID = evt.ContentBlock.ID
				switch evt.ContentBlock.Type {
				case "text":
					emit(types.TextStartDelta{})
				case "thinking":
					signatureBuf = ""
					emit(types.ThinkingStartDelta{})
				case "tool_use":
					toolArgsBuf = toolArgsBuf[:0]
					if isStructuredTool != nil && isStructuredTool(evt.ContentBlock.Name) {
						emit(types.TextStartDelta{})
					} else {
						emit(types.ToolCallStartDelta{
							ID:   evt.ContentBlock.ID,
							Name: evt.ContentBlock.Name,
						})
					}
				case blockServerToolUse:
					// The input streams like a tool call's; the call is
					// reported once it is complete.
					toolArgsBuf = toolArgsBuf[:0]
					serverKinds[evt.ContentBlock.ID] = serverToolKind(evt.ContentBlock.Name)
				default:
					// Server tool results arrive whole in the start event.
					if strings.HasSuffix(evt.ContentBlock.Type, "_tool_result") && evt.ContentBlock.ToolUseID != "" {
						kind, ok := serverKinds[evt.ContentBlock.ToolUseID]
						if !ok {
							kind = serverToolKind(strings.TrimSuffix(evt.ContentBlock.Type, "_tool_result"))
						}
						emit(serverToolResult(evt.ContentBlock.RawJSON(), kind))
					}
				}

			case "content_block_delta":
				switch evt.Delta.Type {
				case "text_delta":
					emit(types.TextContentDelta{Content: evt.Delta.Text})
				case "thinking_delta":
					emit(types.ThinkingContentDelta{Content: evt.Delta.Thinking})
				case "signature_delta":
					signatureBuf += evt.Delta.Signature
				case "input_json_delta":
					toolArgsBuf = append(toolArgsBuf, evt.Delta.PartialJSON...)
					if currentBlockType == blockServerToolUse {
						break
					}
					if isStructuredTool != nil && isStructuredTool(currentBlockName) {
						emit(types.TextContentDelta{Content: evt.Delta.PartialJSON})
					} else {
						emit(types.ToolCallArgumentDelta{ID: currentBlockID, Content: evt.Delta.PartialJSON})
					}
				}

			case "content_block_stop":
				switch currentBlockType {
				case "text":
					emit(types.TextEndDelta{})
				case "thinking":
					emit(types.ThinkingEndDelta{Signature: signatureBuf})
				case "tool_use":
					args, err := streamcheck.DecodeArguments(string(toolArgsBuf))
					if isStructuredTool != nil && isStructuredTool(currentBlockName) {
						if err != nil {
							argsFailure.Set("", "", err)
						}
						emit(types.TextEndDelta{})
					} else if err != nil {
						heldCall = &streamcheck.ArgsFailure{ToolCallID: currentBlockID, Name: currentBlockName, Err: err}
					} else {
						emit(types.ToolCallEndDelta{ID: currentBlockID, Arguments: args})
					}
				case blockServerToolUse:
					// Input that does not decode is reported as absent; the
					// provider ran the call, so there is nothing to refuse.
					input, _ := streamcheck.DecodeArguments(string(toolArgsBuf))
					emit(types.ServerToolCallDelta{ID: currentBlockID, Kind: serverKinds[currentBlockID],
						Name: currentBlockName, Input: input})
				}
				currentBlockType = ""
				currentBlockName = ""
				currentBlockID = ""

			case "message_stop":
				stopped = true

			case "message_delta":
				if string(evt.Delta.StopReason) != "" {
					finishReason = string(evt.Delta.StopReason)
					releaseHeld(!types.IsTruncationFinishReason(finishReason) && !types.IsContentFilterFinishReason(finishReason) &&
						finishReason != stopContextWindowExceeded)
				}
				if evt.Usage.OutputTokens > 0 {
					outputTokens = int(evt.Usage.OutputTokens)
					ud := types.UsageDelta{Cumulative: true,
						CompletionTokens: int(evt.Usage.OutputTokens),
						TotalTokens:      int(evt.Usage.OutputTokens),
						ResponseID:       responseID,
						ResponseModel:    responseModel,
					}
					if finishReason != "" {
						ud.FinishReasons = []string{finishReason}
					}
					emit(ud)
				}
			}
		}

		releaseHeld(false)
		switch err := stream.Err(); {
		case err != nil:
			out <- types.ErrorDelta{Error: classifyAnthropicError(model, err, !emitted)}
		case !stopped && finishReason == "":
			// A clean close without message_stop or stop_reason means the
			// connection ended early; reporting success would hand the loop a
			// partial answer.
			out <- types.ErrorDelta{Error: streamcheck.StreamError("anthropic", model, streamcheck.ErrIncompleteStream, !emitted)}
		case types.IsContentFilterFinishReason(finishReason):
			out <- types.ErrorDelta{Error: streamcheck.Refused("anthropic", model, finishReason)}
		case finishReason == stopContextWindowExceeded:
			// The answer is partial; reporting it as the context limit lets
			// the loop compact or fail instead of taking it as final.
			out <- types.ErrorDelta{Error: &types.ProviderError{Provider: "anthropic", Model: model,
				Kind: types.ErrorKindContextLength, Err: errContextWindowExceeded}}
		case finishReason == stopPauseTurn:
			// The API paused a long server tool turn and expects the partial
			// response sent back to continue it. Server tool blocks are not
			// replayed, so the turn cannot continue: report it rather than
			// hand the loop a partial answer as final.
			out <- types.ErrorDelta{Error: &types.ProviderError{Provider: "anthropic", Model: model,
				Kind: types.ErrorKindPermanent, Err: errPausedTurn}}
		case argsFailure.Failed():
			out <- types.ErrorDelta{Error: argsFailure.Error("anthropic", model, finishReason, outputTokens, maxTokens)}
		}
	}()

	return out
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
//
// Extended thinking rejects a request that ends with an assistant turn, so
// an adapter that thinks does not report assistant prefill. Schema output
// forces a tool call, so an adapter that cannot force one (a manual thinking
// budget, or a model that rejects forcing) does not report structured output.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	caps := catalog.MustLookup("anthropic", string(a.model))
	if a.thinking != nil || a.reasoningEffort != nil || caps.ReasoningDefaultEnabled {
		caps = caps.Without(types.CapAssistantPrefill)
	}
	if a.forcedToolBlocked(caps) != "" {
		caps = caps.Without(types.CapStructuredOutput)
	}
	return caps
}

// ContentSupport implements types.ContentNegotiator.
func (a *Adapter) ContentSupport() types.ContentSupport {
	return types.ContentSupport{
		NativeTypes: map[types.MediaType]bool{
			types.MediaJPEG: true,
			types.MediaPNG:  true,
			types.MediaGIF:  true,
			types.MediaWebP: true,
			types.MediaPDF:  true,
		},
	}
}

// ── Conversion helpers ──────────────────────────────────────────────

func toAnthropicParams(msgs []types.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var system []anthropic.TextBlockParam
	var out []anthropic.MessageParam

	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					// The API rejects an empty text block, so a blank system
					// prompt is dropped; with none left, no system is sent.
					if strings.TrimSpace(bc.Text) != "" {
						system = append(system, anthropic.TextBlockParam{Text: bc.Text})
					}
				case types.ToolResultContent:
					out = appendMsg(out, "user", toToolResultBlock(bc))
				}
			}

		case types.UserMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					out = appendMsg(out, "user", anthropic.NewTextBlock(bc.Text))
				case types.ToolResultContent:
					out = appendMsg(out, "user", toToolResultBlock(bc))
				case types.FileContent:
					if bc.Data != nil && isImageType(bc.MediaType) {
						b64 := base64.StdEncoding.EncodeToString(bc.Data)
						out = appendMsg(out, "user", anthropic.NewImageBlockBase64(string(bc.MediaType), b64))
					} else if bc.Data != nil && bc.MediaType == types.MediaPDF {
						// Native PDF pass-through, matching the ContentSupport claim.
						out = appendMsg(out, "user", anthropic.ContentBlockParamUnion{
							OfDocument: documentBlockFromBytes(bc.Data),
						})
					} else if bc.Data != nil {
						out = appendMsg(out, "user", anthropic.NewTextBlock("[File: "+bc.Filename+"] "+string(bc.Data)))
					}
				}
			}

		case types.AssistantMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.ThinkingContent:
					out = appendMsg(out, "assistant", anthropic.NewThinkingBlock(bc.Signature, bc.Thinking))
				case types.TextContent:
					out = appendMsg(out, "assistant", anthropic.NewTextBlock(bc.Text))
				case types.ToolUseContent:
					out = appendMsg(out, "assistant", anthropic.NewToolUseBlock(bc.ID, bc.Arguments, bc.Name))
				}
			}
		}
	}

	return system, trimPrefill(out)
}

// trimPrefill prepares a request that ends with an assistant turn, which the
// model continues (prefill). The API rejects a final assistant text that ends
// in whitespace, so that whitespace is removed, and a text block left empty
// is dropped.
func trimPrefill(msgs []anthropic.MessageParam) []anthropic.MessageParam {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != anthropic.MessageParamRoleAssistant {
		return msgs
	}
	last := &msgs[len(msgs)-1]
	n := len(last.Content)
	if n == 0 || last.Content[n-1].OfText == nil {
		return msgs
	}
	text := strings.TrimRight(last.Content[n-1].OfText.Text, " \t\r\n")
	if text != "" {
		last.Content[n-1].OfText.Text = text
		return msgs
	}
	last.Content = last.Content[:n-1]
	if len(last.Content) == 0 {
		return msgs[:len(msgs)-1]
	}
	return msgs
}

// appendMsg appends a content block to the last message if same role, otherwise creates new.
func appendMsg(msgs []anthropic.MessageParam, role string, block anthropic.ContentBlockParamUnion) []anthropic.MessageParam {
	r := anthropic.MessageParamRole(role)
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == r {
		msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, block)
		return msgs
	}
	return append(msgs, anthropic.MessageParam{
		Role:    r,
		Content: []anthropic.ContentBlockParamUnion{block},
	})
}

func isImageType(mt types.MediaType) bool {
	switch mt {
	case types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP:
		return true
	}
	return false
}

// toToolResultBlock converts a ToolResultContent into an Anthropic tool_result
// block. When the result has no rich Blocks it takes the exact back-compat path
// (anthropic.NewToolResultBlock). With Blocks it builds a multi-content
// tool_result carrying text, images (base64), and PDF documents; unsupported
// media degrades to a text placeholder so the request never errors.
func toToolResultBlock(c types.ToolResultContent) anthropic.ContentBlockParamUnion {
	if len(c.Blocks) == 0 {
		return anthropic.NewToolResultBlock(c.ToolCallID, c.Text, c.IsError)
	}

	content := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(c.Blocks))
	for _, b := range c.Blocks {
		switch b.Kind {
		case types.ToolResultBlockText:
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: b.Text},
			})
		case types.ToolResultBlockJSON:
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: string(b.JSON)},
			})
		case types.ToolResultBlockImage:
			if b.Data != nil && isImageType(b.MediaType) {
				b64 := base64.StdEncoding.EncodeToString(b.Data)
				content = append(content, anthropic.ToolResultBlockParamContentUnion{
					OfImage: &anthropic.ImageBlockParam{
						Source: anthropic.ImageBlockParamSourceUnion{
							OfBase64: &anthropic.Base64ImageSourceParam{
								Data:      b64,
								MediaType: anthropic.Base64ImageSourceMediaType(b.MediaType),
							},
						},
					},
				})
			} else {
				content = append(content, anthropic.ToolResultBlockParamContentUnion{
					OfText: &anthropic.TextBlockParam{Text: "[image: " + b.Filename + "]"},
				})
			}
		case types.ToolResultBlockFile:
			if b.Data != nil && b.MediaType == types.MediaPDF {
				content = append(content, anthropic.ToolResultBlockParamContentUnion{
					OfDocument: documentBlockFromBytes(b.Data),
				})
			} else {
				content = append(content, anthropic.ToolResultBlockParamContentUnion{
					OfText: &anthropic.TextBlockParam{Text: "[file: " + b.Filename + "]"},
				})
			}
		}
	}

	// Guarantee non-empty content: if every block was dropped, fall back to text.
	if len(content) == 0 {
		return anthropic.NewToolResultBlock(c.ToolCallID, c.Text, c.IsError)
	}
	return anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: c.ToolCallID,
			IsError:   anthropic.Bool(c.IsError),
			Content:   content,
		},
	}
}

// documentBlockFromBytes builds a base64 PDF document block.
func documentBlockFromBytes(data []byte) *anthropic.DocumentBlockParam {
	return &anthropic.DocumentBlockParam{
		Source: anthropic.DocumentBlockParamSourceUnion{
			OfBase64: &anthropic.Base64PDFSourceParam{
				Data: base64.StdEncoding.EncodeToString(data),
			},
		},
	}
}

func toAnthropicTools(defs []types.ToolDef) []anthropic.ToolUnionParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, len(defs))
	for i, d := range defs {
		props := make(map[string]any, len(d.Parameters.Properties))
		for k, v := range d.Parameters.Properties {
			props[k] = propertyToSchema(v)
		}
		out[i] = anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        d.Name,
				Description: anthropic.String(d.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   d.Parameters.Required,
				},
			},
		}
	}
	return out
}

func propertyToSchema(p types.PropertyDef) map[string]any {
	return p.JSONSchema()
}

// streamErrorPrefix is how the SDK reports an error event received inside an
// SSE stream when it cannot decode the event's JSON; the raw JSON follows it.
const streamErrorPrefix = "received error while streaming: "

// classifyAnthropicError maps an error that ended a stream to a ProviderError.
// An HTTP API error is classified by status, message, and Retry-After. An
// error event inside the stream is classified by its error type, for example
// overloaded_error. Anything else is a transport failure, transient only while
// no output has reached the consumer.
func classifyAnthropicError(model string, err error, beforeOutput bool) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode < 300 {
		// The SDK reports an error event inside a 200 stream as an API error
		// carrying the stream's status and the event's JSON.
		return streamcheck.EventError("anthropic", model, apiErr.RawJSON(), err)
	}
	if apiErr != nil {
		var header map[string][]string
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		return streamcheck.HTTPError("anthropic", model, apiErr.StatusCode, header, apiErr.RawJSON(), err)
	}
	if payload, ok := strings.CutPrefix(err.Error(), streamErrorPrefix); ok {
		return streamcheck.EventError("anthropic", model, payload, err)
	}
	return streamcheck.StreamError("anthropic", model, err, beforeOutput)
}
