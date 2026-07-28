package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
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

// Option configures the Google adapter.
type Option func(*Adapter)

// WithVertex targets Vertex AI instead of the Gemini Developer API. The project
// and location are required by that backend; authentication falls back to
// Application Default Credentials, so NewAdapter may be called with an empty
// API key.
//
// The two backends speak the same Gen AI SDK but differ in auth, quota, model
// availability and region, which is why this is a deployment choice rather than
// a model name.
func WithVertex(project, location string) Option {
	return func(a *Adapter) {
		a.backend = genai.BackendVertexAI
		a.project = project
		a.location = location
	}
}

// WithHTTPClient replaces the underlying HTTP client, for callers that need a
// custom transport, timeout, or (on Vertex) their own credentials.
func WithHTTPClient(h *http.Client) Option {
	return func(a *Adapter) { a.httpClient = h }
}

// WithThinkingLevel sets ThinkingConfig.ThinkingLevel, the Gemini 3 way of
// sizing reasoning. Use WithThinkingBudget for 2.5-series models, which take a
// token budget instead: sending the wrong one is silently ignored, so check
// Capabilities for CapReasoningEffort versus CapReasoningBudget.
func WithThinkingLevel(level genai.ThinkingLevel) Option {
	return func(a *Adapter) {
		a.thinking = &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: level}
	}
}

// WithThinkingBudget sets ThinkingConfig.ThinkingBudget in tokens, the
// 2.5-series way of sizing reasoning. A budget of 0 disables thinking on flash
// models; pro models cannot disable it.
func WithThinkingBudget(tokens int32) Option {
	return func(a *Adapter) {
		a.thinking = &genai.ThinkingConfig{IncludeThoughts: tokens > 0, ThinkingBudget: &tokens}
	}
}

// WithoutThinking disables reasoning and its thought output where the model
// permits it.
func WithoutThinking() Option {
	return func(a *Adapter) {
		zero := int32(0)
		a.thinking = &genai.ThinkingConfig{IncludeThoughts: false, ThinkingBudget: &zero}
	}
}

// WithServerTools enables provider-executed tools. Gemini runs search
// grounding and code execution inside the model call, so unlike a local tool
// there is no ToolExecStartDelta, no ToolGate, and no durable step: the only
// trace is the grounding metadata, which this adapter turns into citations.
//
// An unsupported kind is rejected here rather than sent, because Gemini
// answers an unknown tool with an opaque 400.
func WithServerTools(tools ...types.ServerTool) Option {
	return func(a *Adapter) { a.serverTools = append(a.serverTools, tools...) }
}

// WithGoogleSearch enables search grounding, the common case of
// WithServerTools.
func WithGoogleSearch() Option {
	return WithServerTools(types.ServerTool{Kind: types.ServerToolWebSearch})
}

// WithGenerationConfig sets the sampling knobs sent on every request. Fields
// left nil are omitted so the model default applies.
func WithGenerationConfig(g GenerationConfig) Option {
	return func(a *Adapter) { a.generation = g }
}

// WithSafetySettings sets per-request content safety thresholds. Without them
// the backend's defaults apply, which differ between the Gemini API and Vertex.
func WithSafetySettings(settings ...*genai.SafetySetting) Option {
	return func(a *Adapter) { a.safety = settings }
}

// GenerationConfig is the subset of genai.GenerateContentConfig sampling knobs
// callers most often set. Pointer fields are omitted when nil, so the zero
// value sends nothing and the model's defaults apply.
type GenerationConfig struct {
	Temperature     *float32
	TopP            *float32
	TopK            *float32
	Seed            *int32
	MaxOutputTokens int32
	StopSequences   []string
}

// apply copies the set knobs onto a request config.
func (g GenerationConfig) apply(c *genai.GenerateContentConfig) {
	c.Temperature = g.Temperature
	c.TopP = g.TopP
	c.TopK = g.TopK
	c.Seed = g.Seed
	c.MaxOutputTokens = g.MaxOutputTokens
	c.StopSequences = g.StopSequences
}

// Adapter wraps the official Google GenAI SDK client and implements types.Provider,
// types.NamedProvider, types.StructuredOutputProvider, and types.ContentNegotiator.
type Adapter struct {
	client *genai.Client
	model  string

	backend    genai.Backend
	project    string
	location   string
	httpClient *http.Client

	thinking    *genai.ThinkingConfig
	generation  GenerationConfig
	safety      []*genai.SafetySetting
	serverTools []types.ServerTool
}

// NewAdapter creates a new Google provider adapter using the official SDK. It
// targets the Gemini Developer API by default; pass WithVertex to target Vertex
// AI, in which case apiKey may be empty and Application Default Credentials are
// used.
func NewAdapter(ctx context.Context, apiKey, model string, opts ...Option) (*Adapter, error) {
	a := &Adapter{model: model, backend: genai.BackendGeminiAPI}
	for _, o := range opts {
		o(a)
	}
	if a.backend == genai.BackendVertexAI && (a.project == "" || a.location == "") {
		return nil, fmt.Errorf("google: vertex backend requires both project and location")
	}
	// Fail at construction, not mid-stream: an unsupported server tool comes
	// back from Gemini as an opaque 400 on the first request that uses it.
	if err := types.ValidateServerTools(catalog.MustLookup("google", model), a.serverTools); err != nil {
		return nil, fmt.Errorf("google: %w", err)
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     apiKey,
		Backend:    a.backend,
		Project:    a.project,
		Location:   a.location,
		HTTPClient: a.httpClient,
	})
	if err != nil {
		return nil, err
	}
	a.client = client
	return a, nil
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return "google" }

// Model implements types.ModelProvider.
func (a *Adapter) Model() string { return a.model }

// WithModel implements types.ModelSwitcher: it returns a copy of the adapter
// targeting the given model, sharing the underlying client.
func (a *Adapter) WithModel(model string) types.Provider {
	c := *a
	c.model = model
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
	contents, config := a.buildRequest(messages, tools)
	return a.chatStream(ctx, contents, config)
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
func (a *Adapter) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	contents, config := a.buildRequest(messages, tools)
	if schema != nil {
		config.ResponseMIMEType = "application/json"
		config.ResponseSchema = parameterSchemaToGemini(*schema)
	}
	return a.chatStream(ctx, contents, config)
}

// buildRequest converts messages and tools and applies every configured knob,
// so the schema and non-schema paths cannot drift apart.
func (a *Adapter) buildRequest(messages []types.Message, tools []types.ToolDef) ([]*genai.Content, *genai.GenerateContentConfig) {
	systemInst, contents := toGeminiContents(messages)
	config := &genai.GenerateContentConfig{}
	if systemInst != nil {
		config.SystemInstruction = systemInst
	}
	gTools := toGeminiTools(tools)
	gTools = append(gTools, a.serverToolDecls()...)
	if len(gTools) > 0 {
		config.Tools = gTools
	}
	a.generation.apply(config)
	if a.thinking != nil {
		config.ThinkingConfig = a.thinking
	}
	if len(a.safety) > 0 {
		config.SafetySettings = a.safety
	}
	return contents, config
}

// chatStream runs the streaming generation goroutine.
//
// Parts are walked directly rather than read through resp.Text(), because
// Text() skips thought parts entirely: reading it is why reasoning from
// Gemini 2.5 and 3 models used to vanish before reaching the agent loop. Text
// and thinking blocks are bracketed across chunks (one Start, many Content,
// one End) to match the other adapters, so downstream aggregators see one
// block per run of content rather than one per network chunk.
func (a *Adapter) chatStream(ctx context.Context, contents []*genai.Content, config *genai.GenerateContentConfig) (<-chan types.Delta, error) {
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		textStarted, thinkStarted := false, false
		var signature string

		endThinking := func() {
			if thinkStarted {
				out <- types.ThinkingEndDelta{Signature: signature}
				thinkStarted, signature = false, ""
			}
		}
		endText := func() {
			if textStarted {
				out <- types.TextEndDelta{}
				textStarted = false
			}
		}

		for resp, err := range a.client.Models.GenerateContentStream(ctx, a.model, contents, config) {
			if err != nil {
				endThinking()
				endText()
				out <- types.ErrorDelta{Error: &types.ProviderError{
					Provider: "google",
					Model:    a.model,
					Kind:     classifyGoogleError(err),
					Err:      err,
				}}
				return
			}

			if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
				for _, part := range resp.Candidates[0].Content.Parts {
					switch {
					case part.Text != "" && part.Thought:
						endText()
						if !thinkStarted {
							out <- types.ThinkingStartDelta{}
							thinkStarted = true
						}
						if len(part.ThoughtSignature) > 0 {
							signature = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
						}
						out <- types.ThinkingContentDelta{Content: part.Text}

					case part.Text != "":
						endThinking()
						if !textStarted {
							out <- types.TextStartDelta{}
							textStarted = true
						}
						out <- types.TextContentDelta{Content: part.Text}

					case part.FunctionCall != nil:
						endThinking()
						endText()
						id := part.FunctionCall.ID
						if id == "" {
							id = types.NewID()
						}
						out <- types.ToolCallStartDelta{ID: id, Name: part.FunctionCall.Name}
						out <- types.ToolCallEndDelta{Arguments: part.FunctionCall.Args}
					}
				}
			}

			// Emit grounding citations before usage, so a consumer has the
			// sources in hand by the time the turn closes.
			if len(resp.Candidates) > 0 {
				for _, c := range citationsFrom(resp.Candidates[0].GroundingMetadata) {
					out <- types.CitationDelta{Citation: c}
				}
			}

			// Emit usage.
			if resp.UsageMetadata != nil {
				ud := types.UsageDelta{
					PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
					CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
					TotalTokens:      int(resp.UsageMetadata.TotalTokenCount),
					ResponseModel:    resp.ModelVersion,
					ResponseID:       resp.ResponseID,
				}
				if len(resp.Candidates) > 0 && string(resp.Candidates[0].FinishReason) != "" {
					ud.FinishReasons = []string{string(resp.Candidates[0].FinishReason)}
				}
				out <- ud
			}
		}

		endThinking()
		endText()
	}()

	return out, nil
}

// classifyGoogleError maps an SDK error onto a retry decision. Everything used
// to be reported permanent, which meant a 429 or a 503 from Gemini defeated the
// retry and fallback decorators entirely: they only act on transient errors by
// default.
func classifyGoogleError(err error) types.ErrorKind {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return types.ClassifyHTTPStatus(apiErr.Code)
	}
	return types.ErrorKindPermanent
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	return catalog.MustLookup("google", a.model)
}

// serverToolDecls converts the configured server tools into Gemini tool
// declarations.
func (a *Adapter) serverToolDecls() []*genai.Tool {
	var out []*genai.Tool
	for _, st := range a.serverTools {
		switch st.Kind {
		case types.ServerToolWebSearch:
			out = append(out, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
		case types.ServerToolCodeExecution:
			out = append(out, &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}})
		}
	}
	return out
}

// citationsFrom converts Gemini grounding metadata into this SDK's citations,
// so a fact the model found through its own search is attributed the same way
// as one a local search tool found. Without this the grounding is returned,
// dropped, and the answer reads as ungrounded.
func citationsFrom(md *genai.GroundingMetadata) []types.Citation {
	if md == nil {
		return nil
	}
	out := make([]types.Citation, 0, len(md.GroundingChunks))
	for _, chunk := range md.GroundingChunks {
		if chunk == nil || chunk.Web == nil || chunk.Web.URI == "" {
			continue
		}
		c := types.NewCitation(types.CitationWeb, chunk.Web.URI, chunk.Web.Title)
		c.Producer = "google"
		if chunk.Web.Domain != "" {
			c.Meta = map[string]any{"domain": chunk.Web.Domain}
		}
		out = append(out, c)
	}
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

// appendGeminiToolResult emits a function-response content for a tool result
// (merging any JSON block payload into the structured response) plus an inline
// data part for each image/file block carrying bytes.
func appendGeminiToolResult(contents []*genai.Content, bc types.ToolResultContent) []*genai.Content {
	resp := map[string]any{"result": bc.Text}
	if bc.IsError {
		resp = map[string]any{"error": bc.Text}
	}
	for _, b := range bc.Blocks {
		if b.Kind == types.ToolResultBlockJSON && len(b.JSON) > 0 {
			var v any
			if err := json.Unmarshal(b.JSON, &v); err == nil {
				resp["data"] = v
			}
		}
	}
	contents = append(contents, genai.NewContentFromFunctionResponse(bc.ToolCallID, resp, "user"))

	for _, b := range bc.Blocks {
		if (b.Kind == types.ToolResultBlockImage || b.Kind == types.ToolResultBlockFile) && b.Data != nil {
			contents = append(contents, genai.NewContentFromParts(
				[]*genai.Part{genai.NewPartFromBytes(b.Data, string(b.MediaType))}, "user"))
		}
	}
	return contents
}

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
					// Auto-executed tool results are SystemMessages; route them
					// through the same helper as the user path so JSON blocks merge
					// and image/file parts are emitted (not silently dropped).
					contents = appendGeminiToolResult(contents, bc)
				}
			}

		case types.UserMessage:
			var parts []*genai.Part
			for _, c := range v.Content {
				switch bc := c.(type) {
				case types.TextContent:
					parts = append(parts, &genai.Part{Text: bc.Text})
				case types.ToolResultContent:
					contents = appendGeminiToolResult(contents, bc)
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
				case types.ThinkingContent:
					// Thought parts must go back with their signature attached:
					// Gemini 3 uses it to validate the reasoning chain across
					// turns, and dropping it degrades multi-turn function
					// calling. An unparseable signature is sent without one
					// rather than dropping the thought entirely.
					part := &genai.Part{Text: bc.Thinking, Thought: true}
					if sig, err := base64.StdEncoding.DecodeString(bc.Signature); err == nil && len(sig) > 0 {
						part.ThoughtSignature = sig
					}
					parts = append(parts, part)
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
