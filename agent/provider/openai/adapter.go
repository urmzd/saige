package openai

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/internal/generate"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// providerName identifies this adapter in errors, the catalog and metrics.
const providerName = "openai"

// defaultPDFName is the filename sent with an inline PDF that has none;
// OpenAI requires one alongside inline file data.
const defaultPDFName = "document.pdf"

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

// Option configures the OpenAI adapter.
type Option func(*config)

type config struct {
	baseURL     string
	maxRetries  *int
	requestOpts []option.RequestOption
	params      genParams
}

// clientOptions builds the SDK options shared by the chat adapter and the
// embedder. defaultRetries applies when WithMaxRetries was not given; a nil
// default keeps the SDK's own retry count.
func (c *config) clientOptions(apiKey string, defaultRetries *int) []option.RequestOption {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if c.baseURL != "" {
		opts = append(opts, option.WithBaseURL(c.baseURL))
	}
	if n := cmp.Or(c.maxRetries, defaultRetries); n != nil {
		opts = append(opts, option.WithMaxRetries(*n))
	}
	return append(opts, c.requestOpts...)
}

// genParams holds the generation knobs. Every field is a pointer or a slice so
// "unset" is distinguishable from "set to zero": temperature 0 is a meaningful
// value, and sending it when the caller never asked would change behaviour.
type genParams struct {
	cacheKey         string
	cacheRetention   string
	maxTokens        *int64
	temperature      *float64
	topP             *float64
	seed             *int64
	frequencyPenalty *float64
	presencePenalty  *float64
	stop             []string
	reasoningEffort  *string
	parallelTools    *bool
	toolChoice       *types.ToolChoice
}

// WithBaseURL overrides the default OpenAI API base URL. This is also how an
// OpenAI-compatible endpoint (vLLM, Together, Groq, an Azure gateway) is
// targeted, which is worth knowing before reading the capability table: those
// endpoints serve models this catalog has never heard of, so their
// capabilities resolve to the conservative baseline.
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// WithMaxRetries sets how many times the SDK itself retries a failed request.
//
// For the chat adapter the default is 0: retries belong to retry.Provider,
// which counts every attempt, honors Retry-After, and reports RetryError. SDK
// retries run inside one call, hidden from that accounting, and stack under an
// outer retry decorator. Set a positive value only for a bare adapter with no
// retry decorator.
//
// The embedder keeps the SDK default, since no retry decorator wraps
// embedders. Pass 0 to disable it when the caller retries embeddings itself.
func WithMaxRetries(n int) Option {
	return func(c *config) { n = max(n, 0); c.maxRetries = &n }
}

// WithRequestOptions appends SDK request options to every call, for settings
// this package does not wrap (custom headers, an HTTP client, middleware).
// They apply after the adapter's own options, so they can override them.
func WithRequestOptions(opts ...option.RequestOption) Option {
	return func(c *config) { c.requestOpts = append(c.requestOpts, opts...) }
}

// WithMaxTokens caps generated tokens. It is sent as max_completion_tokens,
// the field the reasoning models require; the deprecated max_tokens is never
// used.
func WithMaxTokens(n int64) Option {
	return func(c *config) { c.params.maxTokens = &n }
}

// WithTemperature sets sampling temperature. Unsupported settings are rejected
// by Validate and before either streaming request path sends network traffic.
func WithTemperature(t float64) Option {
	return func(c *config) { c.params.temperature = &t }
}

// WithTopP sets nucleus sampling. Subject to the same reasoning-model caveat
// as WithTemperature.
func WithTopP(p float64) Option {
	return func(c *config) { c.params.topP = &p }
}

// WithSeed requests reproducible sampling.
func WithSeed(seed int64) Option {
	return func(c *config) { c.params.seed = &seed }
}

// WithStopSequences sets sequences that end generation.
func WithStopSequences(stop ...string) Option {
	return func(c *config) { c.params.stop = stop }
}

// WithFrequencyPenalty and WithPresencePenalty set the repetition penalties.
func WithFrequencyPenalty(p float64) Option {
	return func(c *config) { c.params.frequencyPenalty = &p }
}

func WithPresencePenalty(p float64) Option {
	return func(c *config) { c.params.presencePenalty = &p }
}

// WithParallelToolCalls turns parallel tool calling on or off. Turning it off
// matters when the tools are not safe to run concurrently: the model can
// otherwise emit two writes to the same resource in one turn, and the agent
// loop will happily fan them out.
func WithParallelToolCalls(enabled bool) Option {
	return func(c *config) { c.params.parallelTools = &enabled }
}

// WithReasoningEffort sizes reasoning on the models that support it. Legal
// values are in ModelCapabilities.ReasoningEfforts. Unsupported or invalid
// effort values are rejected, including an explicitly empty effort.
func WithReasoningEffort(effort string) Option {
	return func(c *config) { c.params.reasoningEffort = &effort }
}

// Adapter wraps the official OpenAI SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	client openai.Client
	model  openai.ChatModel
	params genParams
}

// NewAdapter creates a new OpenAI provider adapter using the official SDK.
func NewAdapter(apiKey, model string, opts ...Option) *Adapter {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	return &Adapter{
		client: openai.NewClient(cfg.clientOptions(apiKey, new(int))...),
		model:  openai.ChatModel(model),
		params: cfg.params,
	}
}

// applyParams encodes options after Validate has checked their compatibility.
func (a *Adapter) applyParams(p *openai.ChatCompletionNewParams) {
	if a.params.maxTokens != nil {
		p.MaxCompletionTokens = openai.Int(*a.params.maxTokens)
	}
	if a.params.temperature != nil {
		p.Temperature = openai.Float(*a.params.temperature)
	}
	if a.params.topP != nil {
		p.TopP = openai.Float(*a.params.topP)
	}
	if a.params.seed != nil {
		p.Seed = openai.Int(*a.params.seed)
	}
	if a.params.frequencyPenalty != nil {
		p.FrequencyPenalty = openai.Float(*a.params.frequencyPenalty)
	}
	if a.params.presencePenalty != nil {
		p.PresencePenalty = openai.Float(*a.params.presencePenalty)
	}
	if len(a.params.stop) > 0 {
		p.Stop = openai.ChatCompletionNewParamsStopUnion{OfStringArray: a.params.stop}
	}
	if a.params.reasoningEffort != nil {
		p.ReasoningEffort = shared.ReasoningEffort(*a.params.reasoningEffort)
	}
	if a.params.parallelTools != nil {
		p.ParallelToolCalls = openai.Bool(*a.params.parallelTools)
	}
}

// Validate checks configured controls against the currently selected model.
// WithModel preserves options, so switching models revalidates on every call.
func (a *Adapter) Validate() error {
	return a.Capabilities().ValidateOptions(a.EffectiveOptions())
}

// EffectiveOptions implements types.OptionsReporter: the configured controls
// as request options.
func (a *Adapter) EffectiveOptions() types.RequestOptions {
	return types.RequestOptions{
		Temperature: a.params.temperature, TopP: a.params.topP, Seed: a.params.seed,
		MaxOutputTokens: a.params.maxTokens, StopSequences: a.params.stop,
		FrequencyPenalty: a.params.frequencyPenalty, PresencePenalty: a.params.presencePenalty,
		ReasoningEffort: a.params.reasoningEffort, ParallelTools: a.params.parallelTools,
		ToolChoice: a.params.toolChoice,
	}.Clone()
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return providerName }

// Model implements types.ModelProvider.
func (a *Adapter) Model() string { return string(a.model) }

// WithModel implements types.ModelSwitcher: it returns a copy of the adapter
// targeting the given model, sharing the underlying client.
func (a *Adapter) WithModel(model string) types.Provider {
	c := *a
	c.model = openai.ChatModel(model)
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
	return a.chatStream(ctx, messages, tools, nil)
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	var rf *openai.ChatCompletionNewParamsResponseFormatUnion
	if schema != nil {
		schemaMap, strict := responseSchema(*schema)
		rf = &openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "response",
					Schema: schemaMap,
					Strict: openai.Bool(strict),
				},
			},
		}
	}
	return a.chatStream(ctx, messages, tools, rf)
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	return catalog.MustLookup(providerName, string(a.model))
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

func (a *Adapter) chatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef, rf *openai.ChatCompletionNewParamsResponseFormatUnion) (<-chan types.Delta, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if err := a.Capabilities().ValidateRequest(tools, rf != nil); err != nil {
		return nil, err
	}
	if err := a.checkToolChoice(tools); err != nil {
		return nil, err
	}
	toolsEffortNone, err := a.checkChatTools(tools)
	if err != nil {
		return nil, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    a.model,
		Messages: toOpenAIMessages(messages),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}

	a.applyParams(&params)
	if err := a.applyPromptCache(&params); err != nil {
		return nil, err
	}

	oTools := toOpenAITools(tools)
	if len(oTools) > 0 {
		params.Tools = oTools
		a.applyToolChoice(&params)
		if toolsEffortNone {
			params.ReasoningEffort = shared.ReasoningEffort("none")
		}
	}
	if rf != nil {
		params.ResponseFormat = *rf
	}

	stream := a.client.Chat.Completions.NewStreaming(ctx, params)
	return a.consumeStream(stream, rf != nil), nil
}

// toolCallState tracks one streamed tool call by its choice index.
type toolCallState struct {
	id, name string
	args     strings.Builder
	pending  []string // argument fragments that arrived before the call ID
	started  bool
	ended    bool
}

// consumeStream translates the chunk stream into deltas.
//
// Tool calls are tracked by index. A call closes when a later call starts,
// when the choice finishes, or when the stream ends. Its arguments are decoded
// at close. A call whose JSON does not decode and that the model finished
// writing (a later call started, or the choice finished for a reason other
// than "length") closes with ToolCallEndDelta.ArgumentsError set and nil
// Arguments, so the loop refuses it and the model can correct it. The last
// call of a response stopped at "length", or of a stream that ended without a
// finish reason, is never closed: the failure is reported after the stream
// ends as a truncation or an incomplete stream. A structured-output response
// that stopped at "length" is also a truncation, since its JSON is incomplete.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Adapter) consumeStream(stream interface {
	Next() bool
	Current() openai.ChatCompletionChunk
	Err() error
}, structured bool) <-chan types.Delta {
	model := string(a.model)
	maxTokens := 0
	if a.params.maxTokens != nil {
		maxTokens = int(*a.params.maxTokens)
	}
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		textStarted := false
		emitted := false
		emit := func(d types.Delta) {
			if _, usage := d.(types.UsageDelta); !usage {
				emitted = true
			}
			out <- d
		}
		endText := func() {
			if textStarted {
				emit(types.TextEndDelta{})
				textStarted = false
			}
		}

		calls := map[int64]*toolCallState{}
		var order []int64
		var argsFailure streamcheck.ArgsFailure
		// finishReason is read by closeCall, so it is declared before it.
		var finishReason string
		// closeCall closes c. complete reports that the model finished
		// writing c: a later call started, or the choice finished for a
		// reason other than the output limit.
		closeCall := func(c *toolCallState, complete bool) {
			if c.ended || !c.started {
				return
			}
			c.ended = true
			args, err := streamcheck.DecodeArguments(c.args.String())
			switch {
			case err == nil:
				emit(types.ToolCallEndDelta{ID: c.id, Arguments: args})
			case complete:
				emit(types.ToolCallEndDelta{ID: c.id, ArgumentsError: err.Error()})
			default:
				argsFailure.Set(c.id, c.name, err)
			}
		}
		closeAll := func() {
			complete := finishReason != "" && !types.IsTruncationFinishReason(finishReason) && !types.IsContentFilterFinishReason(finishReason)
			for _, idx := range order {
				closeCall(calls[idx], complete)
			}
		}

		var responseID string
		var responseModel string
		var outputTokens int

		for stream.Next() {
			chunk := stream.Current()

			if chunk.ID != "" {
				responseID = chunk.ID
			}
			if chunk.Model != "" {
				responseModel = chunk.Model
			}

			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				delta := choice.Delta

				if delta.Content != "" {
					// Text after a call means the model finished writing it.
					for _, idx := range order {
						closeCall(calls[idx], true)
					}
					if !textStarted {
						emit(types.TextStartDelta{})
						textStarted = true
					}
					emit(types.TextContentDelta{Content: delta.Content})
				}

				for _, tc := range delta.ToolCalls {
					c := calls[tc.Index]
					if c == nil {
						c = &toolCallState{}
						calls[tc.Index] = c
						order = append(order, tc.Index)
					}
					if c.ended {
						continue
					}
					c.name += tc.Function.Name
					if tc.Function.Arguments != "" {
						c.args.WriteString(tc.Function.Arguments)
						if !c.started {
							c.pending = append(c.pending, tc.Function.Arguments)
						}
					}
					if !c.started && tc.ID != "" {
						endText()
						// The model writes calls in sequence, so a new call
						// means every earlier one is complete.
						for _, idx := range order {
							if idx != tc.Index {
								closeCall(calls[idx], true)
							}
						}
						c.id, c.started = tc.ID, true
						emit(types.ToolCallStartDelta{ID: c.id, Name: c.name})
						for _, frag := range c.pending {
							emit(types.ToolCallArgumentDelta{ID: c.id, Content: frag})
						}
						c.pending = nil
						continue
					}
					if c.started && tc.Function.Arguments != "" {
						emit(types.ToolCallArgumentDelta{ID: c.id, Content: tc.Function.Arguments})
					}
				}

				if string(choice.FinishReason) != "" {
					finishReason = string(choice.FinishReason)
					endText()
					closeAll()
				}
			}

			if chunk.Usage.TotalTokens > 0 {
				outputTokens = int(chunk.Usage.CompletionTokens)
				ud := types.UsageDelta{Cumulative: true,
					PromptTokens:       int(chunk.Usage.PromptTokens),
					CachedPromptTokens: int(chunk.Usage.PromptTokensDetails.CachedTokens),
					CompletionTokens:   int(chunk.Usage.CompletionTokens),
					TotalTokens:        int(chunk.Usage.TotalTokens),
					ResponseID:         responseID,
					ResponseModel:      responseModel,
				}
				if finishReason != "" {
					ud.FinishReasons = []string{finishReason}
				}
				emit(ud)
			}
		}

		err := stream.Err()
		endText()
		if err == nil {
			// Close calls left open by a stream that ended without a finish
			// reason, so their decode failures are reported below.
			closeAll()
		}
		switch {
		case err != nil:
			out <- types.ErrorDelta{Error: classifyOpenAIError(model, err, !emitted)}
		case finishReason == "":
			out <- types.ErrorDelta{Error: streamcheck.StreamError(providerName, model, streamcheck.ErrIncompleteStream, !emitted)}
		case types.IsContentFilterFinishReason(finishReason):
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, model, finishReason)}
		case argsFailure.Failed():
			out <- types.ErrorDelta{Error: argsFailure.Error(providerName, model, finishReason, outputTokens, maxTokens)}
		case structured && types.IsTruncationFinishReason(finishReason):
			out <- types.ErrorDelta{Error: streamcheck.Truncated(providerName, model, finishReason, outputTokens, maxTokens)}
		}
	}()

	return out
}

// ── Conversion helpers ──────────────────────────────────────────────

func toOpenAIMessages(msgs []types.Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			var textParts []string
			var toolResults []types.ToolResultContent
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					textParts = append(textParts, bc.Text)
				case types.ToolResultContent:
					toolResults = append(toolResults, bc)
				}
			}
			if len(textParts) > 0 {
				out = append(out, openai.SystemMessage(strings.Join(textParts, "")))
			}
			out = appendOpenAIToolResults(out, toolResults)

		case types.UserMessage:
			var parts []openai.ChatCompletionContentPartUnionParam
			var toolResults []types.ToolResultContent
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					parts = append(parts, openai.TextContentPart(bc.Text))
				case types.ToolResultContent:
					toolResults = append(toolResults, bc)
				case types.FileContent:
					parts = append(parts, fileContentToPart(bc))
				}
			}
			if len(parts) == 1 {
				if tp := parts[0]; tp.OfText != nil {
					out = append(out, openai.UserMessage(tp.OfText.Text))
				} else {
					out = append(out, openai.UserMessage(parts))
				}
			} else if len(parts) > 1 {
				out = append(out, openai.UserMessage(parts))
			}
			out = appendOpenAIToolResults(out, toolResults)

		case types.AssistantMessage:
			var textParts []string
			var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					textParts = append(textParts, bc.Text)
				case types.ToolUseContent:
					argsJSON, _ := json.Marshal(bc.Arguments)
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: bc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      bc.Name,
								Arguments: string(argsJSON),
							},
						},
					})
				}
			}
			assistantMsg := openai.AssistantMessage(strings.Join(textParts, ""))
			if len(toolCalls) > 0 {
				assistantMsg.OfAssistant.ToolCalls = toolCalls
			}
			out = append(out, assistantMsg)
		}
	}
	return out
}

// appendOpenAIToolResults emits a tool message (text projection with placeholders
// for non-text blocks) for each result, plus a follow-up user image message for
// each image block, since OpenAI tool messages accept text only.
func appendOpenAIToolResults(out []openai.ChatCompletionMessageParamUnion, toolResults []types.ToolResultContent) []openai.ChatCompletionMessageParamUnion {
	// First pass: emit ALL tool messages. OpenAI requires the tool messages that
	// answer one assistant tool_calls block to be contiguous, so image follow-ups
	// (which are user messages) must not be interleaved between them.
	for _, tr := range toolResults {
		out = append(out, openai.ToolMessage(openAIToolResultText(tr), tr.ToolCallID))
	}
	// Second pass: emit image follow-ups after every tool message.
	for _, tr := range toolResults {
		for _, b := range tr.Blocks {
			if b.Kind == types.ToolResultBlockImage && b.Data != nil && isImageType(b.MediaType) {
				dataURI := fmt.Sprintf("data:%s;base64,%s", b.MediaType, base64.StdEncoding.EncodeToString(b.Data))
				out = append(out, openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
					openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: dataURI}),
				}))
			}
		}
	}
	return out
}

// openAIToolResultText projects a tool result to text. With no rich blocks it is
// exactly the previous behavior; rich blocks add bracketed placeholders so the
// model is aware of artifacts surfaced as separate image messages.
func openAIToolResultText(tr types.ToolResultContent) string {
	text := tr.Text
	if tr.IsError {
		text = "[TOOL ERROR] " + text
	}
	for _, b := range tr.Blocks {
		switch b.Kind {
		case types.ToolResultBlockImage:
			text += "\n[image: " + b.Filename + "]"
		case types.ToolResultBlockFile:
			text += "\n[file: " + b.Filename + "]"
		case types.ToolResultBlockJSON:
			if tr.Text == "" {
				text += string(b.JSON)
			}
		}
	}
	return text
}

func fileContentToPart(fc types.FileContent) openai.ChatCompletionContentPartUnionParam {
	if fc.Data != nil && isImageType(fc.MediaType) {
		dataURI := fmt.Sprintf("data:%s;base64,%s", fc.MediaType, base64.StdEncoding.EncodeToString(fc.Data))
		return openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL: dataURI,
		})
	}
	if fc.URI != "" && isImageType(fc.MediaType) {
		return openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL: fc.URI,
		})
	}
	if fc.Data != nil && fc.MediaType == types.MediaPDF {
		// Native PDF pass-through, matching the ContentSupport claim. OpenAI
		// requires a filename alongside inline file_data.
		name := fc.Filename
		if name == "" {
			name = defaultPDFName
		}
		return openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
			FileData: openai.String(fmt.Sprintf("data:%s;base64,%s", fc.MediaType, base64.StdEncoding.EncodeToString(fc.Data))),
			Filename: openai.String(name),
		})
	}
	if format, ok := audioFormat(fc.MediaType); ok && fc.Data != nil {
		// Audio-input models take base64 audio in an input_audio part. Sent
		// as text, the raw bytes would reach the model as noise.
		return openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{
			Data:   base64.StdEncoding.EncodeToString(fc.Data),
			Format: format,
		})
	}
	desc := fmt.Sprintf("[File: %s, type: %s]", fc.Filename, fc.MediaType)
	if fc.Data != nil && isTextType(fc.MediaType) {
		desc = fmt.Sprintf("[File: %s, type: %s]\n%s", fc.Filename, fc.MediaType, string(fc.Data))
	}
	return openai.TextContentPart(desc)
}

// audioFormat returns the input_audio format name for a media type.
func audioFormat(mt types.MediaType) (string, bool) {
	switch mt {
	case types.MediaWAV:
		return "wav", true
	case types.MediaMP3:
		return "mp3", true
	}
	return "", false
}

// isTextType reports whether inline file bytes can be sent as text. Other
// binary payloads (video, office documents) are described by a placeholder
// instead, since their bytes are not readable text.
func isTextType(mt types.MediaType) bool {
	switch mt {
	case types.MediaText, types.MediaCSV, types.MediaJSON, types.MediaHTML, "":
		return true
	}
	return strings.HasPrefix(string(mt), "text/")
}

func isImageType(mt types.MediaType) bool {
	switch mt {
	case types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP:
		return true
	}
	return false
}

func toOpenAITools(defs []types.ToolDef) []openai.ChatCompletionToolUnionParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, len(defs))
	for i, d := range defs {
		out[i] = openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
			Name:        d.Name,
			Description: openai.String(d.Description),
			Parameters:  openai.FunctionParameters(parameterSchemaToMap(d.Parameters)),
		})
	}
	return out
}

func parameterSchemaToMap(ps types.ParameterSchema) map[string]any {
	schema := map[string]any{"type": ps.Type}
	if len(ps.Required) > 0 {
		schema["required"] = ps.Required
	}
	if len(ps.Properties) > 0 {
		props := make(map[string]any, len(ps.Properties))
		for k, v := range ps.Properties {
			props[k] = propertyToSchema(v)
		}
		schema["properties"] = props
	}
	return schema
}

// responseSchema builds the response_format schema and reports whether it can
// be sent in strict mode.
//
// Strict mode makes the API guarantee the shape, but it has two requirements
// of its own: every object must set additionalProperties to false, and every
// object must list all of its properties as required. The first is added
// here, since a ParameterSchema has no way to say otherwise. The second is the
// caller's choice, so a schema with optional properties is sent non-strict
// rather than rejected: the API then treats the schema as guidance.
func responseSchema(ps types.ParameterSchema) (map[string]any, bool) {
	schema := parameterSchemaToMap(ps)
	return schema, closeObjects(schema)
}

// closeObjects sets additionalProperties to false on every object in the
// schema, in place, and reports whether every object requires all of its
// properties.
func closeObjects(node map[string]any) bool {
	strict := true
	object := node["type"] == "object"
	if union, ok := node["type"].([]string); ok {
		for _, member := range union {
			object = object || member == "object"
		}
	}
	if object {
		node["additionalProperties"] = false
		props, _ := node["properties"].(map[string]any)
		required := map[string]bool{}
		switch list := node["required"].(type) {
		case []string:
			for _, name := range list {
				required[name] = true
			}
		}
		for name, child := range props {
			if !required[name] {
				strict = false
			}
			if m, ok := child.(map[string]any); ok && !closeObjects(m) {
				strict = false
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok && !closeObjects(items) {
		strict = false
	}
	return strict
}

func propertyToSchema(p types.PropertyDef) map[string]any {
	return p.JSONSchema()
}

// classifyOpenAIError maps an error to a ProviderError. An HTTP API error is
// classified by status, message, and Retry-After. An error event inside the
// stream is classified by its error type or code. Anything else is a transport
// failure, transient only while no output has reached the consumer.
func classifyOpenAIError(model string, err error, beforeOutput bool) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		var header http.Header
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		msg := apiErr.Message
		if msg == "" {
			msg = apiErr.RawJSON()
		}
		if apiErr.Code != "" {
			msg = apiErr.Code + ": " + msg
		}
		return streamcheck.HTTPError(providerName, model, apiErr.StatusCode, header, msg, err)
	}
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		return streamcheck.EventError(providerName, model, string(streamErr.Event.Data), err)
	}
	return streamcheck.StreamError(providerName, model, err, beforeOutput)
}
