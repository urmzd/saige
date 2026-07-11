package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/urmzd/saige/agent/types"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
	_ types.ModelProvider            = (*Adapter)(nil)
)

// Option configures the OpenAI adapter.
type Option func(*config)

type config struct {
	baseURL string
}

// WithBaseURL overrides the default OpenAI API base URL.
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// Adapter wraps the official OpenAI SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	client openai.Client
	model  openai.ChatModel
}

// NewAdapter creates a new OpenAI provider adapter using the official SDK.
func NewAdapter(apiKey, model string, opts ...Option) *Adapter {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if cfg.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(cfg.baseURL))
	}
	return &Adapter{
		client: openai.NewClient(clientOpts...),
		model:  openai.ChatModel(model),
	}
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return "openai" }

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
	return types.GenerateText(ctx, a, prompt)
}

// ChatStream implements types.Provider.
func (a *Adapter) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return a.chatStream(ctx, messages, tools, nil)
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	var rf *openai.ChatCompletionNewParamsResponseFormatUnion
	if schema != nil {
		schemaMap := parameterSchemaToMap(*schema)
		rf = &openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "response",
					Schema: schemaMap,
					Strict: openai.Bool(true),
				},
			},
		}
	}
	return a.chatStream(ctx, messages, tools, rf)
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
	params := openai.ChatCompletionNewParams{
		Model:    a.model,
		Messages: toOpenAIMessages(messages),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}

	oTools := toOpenAITools(tools)
	if len(oTools) > 0 {
		params.Tools = oTools
	}
	if rf != nil {
		params.ResponseFormat = *rf
	}

	stream := a.client.Chat.Completions.NewStreaming(ctx, params)

	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		acc := openai.ChatCompletionAccumulator{}
		textStarted := false
		startedToolCalls := make(map[int64]bool)

		var responseID string
		var responseModel string
		var finishReason string

		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)

			if chunk.ID != "" {
				responseID = chunk.ID
			}
			if chunk.Model != "" {
				responseModel = chunk.Model
			}

			if chunk.Usage.TotalTokens > 0 {
				ud := types.UsageDelta{
					PromptTokens:     int(chunk.Usage.PromptTokens),
					CompletionTokens: int(chunk.Usage.CompletionTokens),
					TotalTokens:      int(chunk.Usage.TotalTokens),
					ResponseID:       responseID,
					ResponseModel:    responseModel,
				}
				if finishReason != "" {
					ud.FinishReasons = []string{finishReason}
				}
				out <- ud
			}

			if len(chunk.Choices) == 0 {
				continue
			}

			choice := chunk.Choices[0]
			if string(choice.FinishReason) != "" {
				finishReason = string(choice.FinishReason)
			}
			delta := choice.Delta

			if delta.Content != "" {
				if !textStarted {
					out <- types.TextStartDelta{}
					textStarted = true
				}
				out <- types.TextContentDelta{Content: delta.Content}
			}

			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				if !startedToolCalls[idx] {
					if textStarted {
						out <- types.TextEndDelta{}
						textStarted = false
					}
					if tc.ID != "" {
						startedToolCalls[idx] = true
						out <- types.ToolCallStartDelta{ID: tc.ID, Name: tc.Function.Name}
					}
				}
				if tc.Function.Arguments != "" {
					out <- types.ToolCallArgumentDelta{Content: tc.Function.Arguments}
				}
			}

			// FinishedChatCompletionToolCall embeds ChatCompletionMessageFunctionToolCallFunction
			// which has Arguments and Name fields directly.
			if finishedTC, ok := acc.JustFinishedToolCall(); ok {
				var args map[string]any
				if finishedTC.Arguments != "" {
					_ = json.Unmarshal([]byte(finishedTC.Arguments), &args)
				}
				out <- types.ToolCallEndDelta{Arguments: args}
			}

			if _, ok := acc.JustFinishedContent(); ok {
				if textStarted {
					out <- types.TextEndDelta{}
					textStarted = false
				}
			}
		}

		if err := stream.Err(); err != nil {
			out <- types.ErrorDelta{Error: classifyOpenAIError(err)}
		}

		if textStarted {
			out <- types.TextEndDelta{}
		}
	}()

	return out, nil
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
			name = "document.pdf"
		}
		return openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
			FileData: openai.String(fmt.Sprintf("data:%s;base64,%s", fc.MediaType, base64.StdEncoding.EncodeToString(fc.Data))),
			Filename: openai.String(name),
		})
	}
	desc := fmt.Sprintf("[File: %s, type: %s]", fc.Filename, fc.MediaType)
	if fc.Data != nil {
		desc = fmt.Sprintf("[File: %s, type: %s]\n%s", fc.Filename, fc.MediaType, string(fc.Data))
	}
	return openai.TextContentPart(desc)
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
		props := make(map[string]any, len(p.Properties))
		for k, v := range p.Properties {
			props[k] = propertyToSchema(v)
		}
		m["properties"] = props
	}
	if len(p.Required) > 0 {
		m["required"] = p.Required
	}
	return m
}

func classifyOpenAIError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &types.ProviderError{
			Provider: "openai",
			Kind:     types.ClassifyHTTPStatus(apiErr.StatusCode),
			Code:     apiErr.StatusCode,
			Err:      err,
		}
	}
	return &types.ProviderError{
		Provider: "openai",
		Kind:     types.ErrorKindPermanent,
		Err:      err,
	}
}
