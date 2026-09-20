package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
	_ types.ModelProvider            = (*Adapter)(nil)
	_ types.ModelSwitcher            = (*Adapter)(nil)
	_ types.CapabilityReporter       = (*Adapter)(nil)
	_ types.ContentNegotiator        = (*Adapter)(nil)
)

// Adapter wraps the official Anthropic SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	systemCacheTTL  string
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
// "off" as disable_parallel_tool_use on the auto tool choice, so this is only
// applied when the request does not already force a specific tool: the
// structured-output path forces one, and overriding that would break it.
func WithParallelToolCalls(enabled bool) Option {
	return func(a *Adapter) { a.parallelTools = &enabled }
}

// WithBaseURL overrides the API base URL, for gateways and proxies.
func WithBaseURL(url string) Option {
	return func(a *Adapter) { a.baseURL = url }
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
	if a.parallelTools != nil && p.ToolChoice.OfAuto == nil && p.ToolChoice.OfTool == nil {
		p.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{
				DisableParallelToolUse: anthropic.Bool(!*a.parallelTools),
			},
		}
	}
}

// Validate rejects unsupported or incompatible controls before any request.
func (a *Adapter) Validate() error {
	caps := a.Capabilities()
	var topK *float64
	if a.topK != nil {
		k := float64(*a.topK)
		topK = &k
	}
	o := types.RequestOptions{Temperature: a.temperature, TopP: a.topP, TopK: topK,
		MaxOutputTokens: &a.maxTokens, StopSequences: a.stop, ParallelTools: a.parallelTools,
		ReasoningBudget: a.thinking, ReasoningEffort: a.reasoningEffort}
	if err := caps.ValidateOptions(o); err != nil {
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
	return types.GenerateText(ctx, a, prompt)
}

// ChatStream implements types.Provider.
func (a *Adapter) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	if err := a.Capabilities().ValidateRequest(tools, false); err != nil {
		return nil, err
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}

	systemBlocks, aMsgs := toAnthropicParams(messages)
	if err := a.applyPromptCache(systemBlocks); err != nil {
		return nil, err
	}
	aTools := toAnthropicTools(tools)

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

	stream := a.client.Messages.NewStreaming(ctx, params)
	return a.consumeStream(stream, nil), nil
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
// This adapter constrains output with a hidden tool and forces the model to call it.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	if err := a.Capabilities().ValidateRequest(tools, schema != nil); err != nil {
		return nil, err
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}

	if schema != nil && a.thinking != nil {
		return nil, a.Capabilities().OptionError("structured_output", "forced-tool schema output is incompatible with manual thinking")
	}
	if schema != nil {
		if err := a.Capabilities().Require(types.CapTools, types.CapToolChoice); err != nil {
			return nil, err
		}
	}

	systemBlocks, aMsgs := toAnthropicParams(messages)
	if err := a.applyPromptCache(systemBlocks); err != nil {
		return nil, err
	}
	aTools := toAnthropicTools(tools)

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

	isStructured := func(name string) bool {
		return schema != nil && name == "structured_output"
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	return a.consumeStream(stream, isStructured), nil
}

// consumeStream reads from the Anthropic streaming response and emits deltas.
// If isStructuredTool is non-nil and returns true for a tool_use block name,
// the tool's input JSON is emitted as text deltas instead of tool call deltas.
func (a *Adapter) consumeStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], isStructuredTool func(string) bool) <-chan types.Delta {
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		var currentBlockType string
		var currentBlockName string
		var toolArgsBuf []byte
		var signatureBuf string

		// Track response metadata for the final UsageDelta.
		var responseID string
		var responseModel string
		var finishReason string

		for stream.Next() {
			evt := stream.Current()

			switch evt.Type {
			case "message_start":
				responseID = evt.Message.ID
				responseModel = string(evt.Message.Model)
				if evt.Message.Usage.InputTokens+evt.Message.Usage.CacheReadInputTokens+evt.Message.Usage.CacheCreationInputTokens > 0 {
					out <- types.UsageDelta{
						PromptTokens:       int(evt.Message.Usage.InputTokens + evt.Message.Usage.CacheReadInputTokens + evt.Message.Usage.CacheCreationInputTokens),
						CachedPromptTokens: int(evt.Message.Usage.CacheReadInputTokens),
						CacheWriteTokens:   int(evt.Message.Usage.CacheCreationInputTokens),
						TotalTokens:        int(evt.Message.Usage.InputTokens + evt.Message.Usage.CacheReadInputTokens + evt.Message.Usage.CacheCreationInputTokens + evt.Message.Usage.OutputTokens),
						ResponseID:         evt.Message.ID,
						ResponseModel:      string(evt.Message.Model),
					}
				}

			case "content_block_start":
				currentBlockType = evt.ContentBlock.Type
				currentBlockName = evt.ContentBlock.Name
				switch evt.ContentBlock.Type {
				case "text":
					out <- types.TextStartDelta{}
				case "thinking":
					signatureBuf = ""
					out <- types.ThinkingStartDelta{}
				case "tool_use":
					toolArgsBuf = toolArgsBuf[:0]
					if isStructuredTool != nil && isStructuredTool(evt.ContentBlock.Name) {
						out <- types.TextStartDelta{}
					} else {
						out <- types.ToolCallStartDelta{
							ID:   evt.ContentBlock.ID,
							Name: evt.ContentBlock.Name,
						}
					}
				}

			case "content_block_delta":
				switch evt.Delta.Type {
				case "text_delta":
					out <- types.TextContentDelta{Content: evt.Delta.Text}
				case "thinking_delta":
					out <- types.ThinkingContentDelta{Content: evt.Delta.Thinking}
				case "signature_delta":
					signatureBuf += evt.Delta.Signature
				case "input_json_delta":
					toolArgsBuf = append(toolArgsBuf, evt.Delta.PartialJSON...)
					if isStructuredTool != nil && isStructuredTool(currentBlockName) {
						out <- types.TextContentDelta{Content: evt.Delta.PartialJSON}
					} else {
						out <- types.ToolCallArgumentDelta{Content: evt.Delta.PartialJSON}
					}
				}

			case "content_block_stop":
				switch currentBlockType {
				case "text":
					out <- types.TextEndDelta{}
				case "thinking":
					out <- types.ThinkingEndDelta{Signature: signatureBuf}
				case "tool_use":
					if isStructuredTool != nil && isStructuredTool(currentBlockName) {
						out <- types.TextEndDelta{}
					} else {
						var args map[string]any
						if len(toolArgsBuf) > 0 {
							_ = json.Unmarshal(toolArgsBuf, &args)
						}
						out <- types.ToolCallEndDelta{Arguments: args}
					}
				}
				currentBlockType = ""
				currentBlockName = ""

			case "message_delta":
				if string(evt.Delta.StopReason) != "" {
					finishReason = string(evt.Delta.StopReason)
				}
				if evt.Usage.OutputTokens > 0 {
					ud := types.UsageDelta{
						CompletionTokens: int(evt.Usage.OutputTokens),
						TotalTokens:      int(evt.Usage.OutputTokens),
						ResponseID:       responseID,
						ResponseModel:    responseModel,
					}
					if finishReason != "" {
						ud.FinishReasons = []string{finishReason}
					}
					out <- ud
				}
			}
		}

		if err := stream.Err(); err != nil {
			out <- types.ErrorDelta{Error: classifyAnthropicError(err)}
		}
	}()

	return out
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	return catalog.MustLookup("anthropic", string(a.model))
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
					system = append(system, anthropic.TextBlockParam{Text: bc.Text})
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

	return system, out
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

func classifyAnthropicError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return &types.ProviderError{
			Provider: "anthropic",
			Kind:     types.ClassifyHTTPStatus(apiErr.StatusCode),
			Code:     apiErr.StatusCode,
			Err:      err,
		}
	}
	return &types.ProviderError{
		Provider: "anthropic",
		Kind:     types.ErrorKindPermanent,
		Err:      err,
	}
}
