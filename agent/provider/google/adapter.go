package google

import (
	"context"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
)

// Adapter wraps the official Google GenAI SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	client *genai.Client
	model  string
}

// NewAdapter creates a new Google provider adapter using the official SDK.
func NewAdapter(ctx context.Context, apiKey, model string) (*Adapter, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &Adapter{client: client, model: model}, nil
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return "google" }

// ChatStream implements types.Provider.
func (a *Adapter) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	systemInst, contents := toGeminiContents(messages)
	config := &genai.GenerateContentConfig{}
	if systemInst != nil {
		config.SystemInstruction = systemInst
	}

	gTools := toGeminiTools(tools)
	if len(gTools) > 0 {
		config.Tools = gTools
	}

	return a.chatStream(ctx, contents, config)
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	systemInst, contents := toGeminiContents(messages)
	config := &genai.GenerateContentConfig{}
	if systemInst != nil {
		config.SystemInstruction = systemInst
	}

	gTools := toGeminiTools(tools)
	if len(gTools) > 0 {
		config.Tools = gTools
	}

	if schema != nil {
		config.ResponseMIMEType = "application/json"
		config.ResponseSchema = parameterSchemaToGemini(*schema)
	}

	return a.chatStream(ctx, contents, config)
}

// chatStream runs the streaming generation goroutine.
func (a *Adapter) chatStream(ctx context.Context, contents []*genai.Content, config *genai.GenerateContentConfig) (<-chan types.Delta, error) {
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		for resp, err := range a.client.Models.GenerateContentStream(ctx, a.model, contents, config) {
			if err != nil {
				out <- types.ErrorDelta{Error: &types.ProviderError{
					Provider: "google",
					Model:    a.model,
					Kind:     types.ErrorKindPermanent,
					Err:      err,
				}}
				return
			}

			// Emit text content.
			if text := resp.Text(); text != "" {
				out <- types.TextStartDelta{}
				out <- types.TextContentDelta{Content: text}
				out <- types.TextEndDelta{}
			}

			// Emit function calls (Gemini sends complete calls per chunk).
			for _, fc := range resp.FunctionCalls() {
				id := fc.ID
				if id == "" {
					id = types.NewID()
				}
				out <- types.ToolCallStartDelta{ID: id, Name: fc.Name}
				out <- types.ToolCallEndDelta{Arguments: fc.Args}
			}

			// Emit usage.
			if resp.UsageMetadata != nil {
				out <- types.UsageDelta{
					PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
					CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
					TotalTokens:      int(resp.UsageMetadata.TotalTokenCount),
				}
			}
		}
	}()

	return out, nil
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

func toGeminiContents(msgs []types.Message) (*genai.Content, []*genai.Content) {
	var systemParts []*genai.Part
	var contents []*genai.Content

	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					systemParts = append(systemParts, &genai.Part{Text: bc.Text})
				case types.ToolResultContent:
					// Tool results go as function responses from "user" role.
					resp := map[string]any{"result": bc.Text}
					if bc.IsError {
						resp["error"] = bc.Text
						delete(resp, "result")
					}
					contents = append(contents, genai.NewContentFromFunctionResponse(
						bc.ToolCallID, resp, "user",
					))
				}
			}

		case types.UserMessage:
			var parts []*genai.Part
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					parts = append(parts, &genai.Part{Text: bc.Text})
				case types.ToolResultContent:
					resp := map[string]any{"result": bc.Text}
					if bc.IsError {
						resp["error"] = bc.Text
						delete(resp, "result")
					}
					contents = append(contents, genai.NewContentFromFunctionResponse(
						bc.ToolCallID, resp, "user",
					))
				case types.FileContent:
					if bc.Data != nil {
						parts = append(parts, &genai.Part{
							InlineData: &genai.Blob{
								Data:     bc.Data,
								MIMEType: string(bc.MediaType),
							},
						})
					}
				}
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, "user"))
			}

		case types.AssistantMessage:
			var parts []*genai.Part
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					parts = append(parts, &genai.Part{Text: bc.Text})
				case types.ToolUseContent:
					parts = append(parts, &genai.Part{
						FunctionCall: &genai.FunctionCall{
							Name: bc.Name,
							Args: bc.Arguments,
						},
					})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, "model"))
			}
		}
	}

	var systemInst *genai.Content
	if len(systemParts) > 0 {
		systemInst = &genai.Content{Parts: systemParts}
	}
	return systemInst, contents
}

func toGeminiTools(defs []types.ToolDef) []*genai.Tool {
	if len(defs) == 0 {
		return nil
	}
	funcs := make([]*genai.FunctionDeclaration, len(defs))
	for i, d := range defs {
		funcs[i] = &genai.FunctionDeclaration{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  parameterSchemaToGemini(d.Parameters),
		}
	}
	return []*genai.Tool{{FunctionDeclarations: funcs}}
}

func parameterSchemaToGemini(ps types.ParameterSchema) *genai.Schema {
	s := &genai.Schema{
		Type:     mapType(ps.Type),
		Required: ps.Required,
	}
	if len(ps.Properties) > 0 {
		s.Properties = make(map[string]*genai.Schema, len(ps.Properties))
		for k, v := range ps.Properties {
			s.Properties[k] = propertyToGemini(v)
		}
	}
	return s
}

func propertyToGemini(p types.PropertyDef) *genai.Schema {
	s := &genai.Schema{
		Type:        mapType(p.Type),
		Description: p.Description,
		Enum:        p.Enum,
		Required:    p.Required,
		Default:     p.Default,
	}
	if p.Items != nil {
		s.Items = propertyToGemini(*p.Items)
	}
	if len(p.Properties) > 0 {
		s.Properties = make(map[string]*genai.Schema, len(p.Properties))
		for k, v := range p.Properties {
			s.Properties[k] = propertyToGemini(v)
		}
	}
	return s
}

func mapType(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	default:
		return genai.TypeString
	}
}
