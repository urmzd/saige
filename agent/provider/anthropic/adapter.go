package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/urmzd/saige/agent/types"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
)

// Adapter wraps the official Anthropic SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
	thinking  *int64 // nil = disabled; set to budget tokens to enable extended thinking
}

// Option configures the Anthropic adapter.
type Option func(*Adapter)

// WithMaxTokens sets the max tokens for responses.
func WithMaxTokens(n int64) Option {
	return func(a *Adapter) { a.maxTokens = n }
}

// WithThinking enables extended thinking with the given token budget.
// Requires a minimum budget of 1024 tokens and a model that supports
// extended thinking (e.g. claude-sonnet-4-5-20250514).
func WithThinking(budgetTokens int64) Option {
	return func(a *Adapter) { a.thinking = &budgetTokens }
}

// NewAdapter creates a new Anthropic provider adapter using the official SDK.
func NewAdapter(apiKey, model string, opts ...Option) *Adapter {
	a := &Adapter{
		client:    anthropic.NewClient(option.WithAPIKey(apiKey)),
		model:     anthropic.Model(model),
		maxTokens: 4096,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return "anthropic" }

// ChatStream implements types.Provider.
func (a *Adapter) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	systemBlocks, aMsgs := toAnthropicParams(messages)
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
	if a.thinking != nil {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(*a.thinking)
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	return a.consumeStream(stream, nil), nil
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
// Anthropic has no native response_format; we inject a hidden tool and force the model to call it.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	systemBlocks, aMsgs := toAnthropicParams(messages)
	aTools := toAnthropicTools(tools)

	params := anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  aMsgs,
		System:    systemBlocks,
	}
	if a.thinking != nil {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(*a.thinking)
	}

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

		for stream.Next() {
			evt := stream.Current()

			switch evt.Type {
			case "message_start":
				if evt.Message.Usage.InputTokens > 0 {
					out <- types.UsageDelta{
						PromptTokens: int(evt.Message.Usage.InputTokens),
						TotalTokens:  int(evt.Message.Usage.InputTokens + evt.Message.Usage.OutputTokens),
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
				if evt.Usage.OutputTokens > 0 {
					out <- types.UsageDelta{
						CompletionTokens: int(evt.Usage.OutputTokens),
						TotalTokens:      int(evt.Usage.OutputTokens),
					}
				}
			}
		}

		if err := stream.Err(); err != nil {
			out <- types.ErrorDelta{Error: classifyAnthropicError(err)}
		}
	}()

	return out
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
					block := anthropic.NewToolResultBlock(bc.ToolCallID, bc.Text, bc.IsError)
					out = appendMsg(out, "user", block)
				}
			}

		case types.UserMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					out = appendMsg(out, "user", anthropic.NewTextBlock(bc.Text))
				case types.ToolResultContent:
					out = appendMsg(out, "user", anthropic.NewToolResultBlock(bc.ToolCallID, bc.Text, bc.IsError))
				case types.FileContent:
					if bc.Data != nil && isImageType(bc.MediaType) {
						b64 := base64.StdEncoding.EncodeToString(bc.Data)
						out = appendMsg(out, "user", anthropic.NewImageBlockBase64(string(bc.MediaType), b64))
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
				},
			},
		}
	}
	return out
}

func propertyToSchema(p types.PropertyDef) map[string]any {
	m := map[string]any{"type": p.Type}
	if p.Description != "" {
		m["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		m["enum"] = p.Enum
	}
	if p.Default != nil {
		m["default"] = p.Default
	}
	if p.Items != nil {
		m["items"] = propertyToSchema(*p.Items)
	}
	if len(p.Properties) > 0 {
		nested := make(map[string]any, len(p.Properties))
		for k, v := range p.Properties {
			nested[k] = propertyToSchema(v)
		}
		m["properties"] = nested
	}
	if len(p.Required) > 0 {
		m["required"] = p.Required
	}
	return m
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
