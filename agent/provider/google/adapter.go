package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/auth"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/internal/generate"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// providerName identifies this adapter in errors, the catalog and metrics.
const providerName = "google"

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
	_ types.ModelProvider            = (*Adapter)(nil)
	_ types.TargetSwitcher           = (*Adapter)(nil)
	_ types.CapabilityReporter       = (*Adapter)(nil)
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
		a.backend.kind = genai.BackendVertexAI
		a.backend.project = project
		a.backend.location = location
	}
}

// WithHTTPClient replaces the underlying HTTP client, for callers that need a
// custom transport or timeout. On Vertex, a client given without
// WithCredentials must authenticate by itself.
func WithHTTPClient(h *http.Client) Option {
	return func(a *Adapter) { a.backend.httpClient = h }
}

// WithCredentials sets the Google Cloud credentials Vertex requests carry,
// in place of Application Default Credentials.
func WithCredentials(c *auth.Credentials) Option {
	return func(a *Adapter) { a.backend.credentials = c }
}

// WithThinkingLevel sets ThinkingConfig.ThinkingLevel, the Gemini 3 way of
// sizing reasoning. Use WithThinkingBudget for 2.5-series models, which take a
// token budget instead: sending the wrong one is rejected locally; check
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
		a.thinking = &genai.ThinkingConfig{IncludeThoughts: tokens != 0, ThinkingBudget: &tokens}
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
// there is no ToolExecStartDelta, no ToolGate, and no durable step. The trace
// is reported as ServerToolCallPart and ServerToolResultPart pairs: the
// generated code and its output, and the search queries with the sources
// found. Grounding sources are also reported as CitationPart parts.
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

// WithResponseModalities sets the kinds of output the model returns, such
// as genai.ModalityImage on an image model or genai.ModalityAudio on a
// speech model. Generated images stream as ImageOutPart parts and audio as
// AudioOutPart parts, whose bytes arrive in PartDelta.Data chunks. A model
// that cannot produce a requested modality fails the request.
func WithResponseModalities(m ...genai.Modality) Option {
	return func(a *Adapter) { a.modalities = append([]genai.Modality(nil), m...) }
}

// WithSpeechConfig sets the voice of audio output.
func WithSpeechConfig(c *genai.SpeechConfig) Option {
	return func(a *Adapter) { a.speech = c }
}

// GenerationConfig is the subset of genai.GenerateContentConfig sampling knobs
// callers most often set. Pointer fields are omitted when nil, so the zero
// value sends nothing and the model's defaults apply.
type GenerationConfig struct {
	Temperature      *float32
	TopP             *float32
	TopK             *float32
	Seed             *int32
	MaxOutputTokens  int32
	StopSequences    []string
	FrequencyPenalty *float32
	PresencePenalty  *float32
}

// apply copies the set knobs onto a request config.
func (g GenerationConfig) apply(c *genai.GenerateContentConfig) {
	c.Temperature = g.Temperature
	c.TopP = g.TopP
	c.TopK = g.TopK
	c.Seed = g.Seed
	c.MaxOutputTokens = g.MaxOutputTokens
	c.StopSequences = g.StopSequences
	c.FrequencyPenalty = g.FrequencyPenalty
	c.PresencePenalty = g.PresencePenalty
}

// Adapter wraps the official Google GenAI SDK client and implements types.Provider,
// types.NamedProvider and types.StructuredOutputProvider.
type Adapter struct {
	contextCache *ContextCache
	client       *genai.Client
	model        string

	backend backend

	thinking    *genai.ThinkingConfig
	generation  GenerationConfig
	safety      []*genai.SafetySetting
	serverTools []types.ServerTool
	toolChoice  *types.ToolChoice
	dials       []types.DialLayer
	dialPolicy  *types.DialPolicy
	modalities  []genai.Modality
	speech      *genai.SpeechConfig
}

// Config names the account and model an adapter or embedder serves.
type Config struct {
	// APIKey authenticates Gemini Developer API requests. With WithVertex it
	// may be empty, and Application Default Credentials are used.
	APIKey string
	// Model is the model requests go to. Required.
	Model types.ModelID
}

// New creates a Google provider adapter using the official SDK. It targets
// the Gemini Developer API by default; pass WithVertex to target Vertex AI.
// ctx bounds the credential lookup. A missing model, an incomplete Vertex
// target, or controls the model does not take are errors; the first two
// wrap types.ErrInvalidConfig.
func New(ctx context.Context, cfg Config, opts ...Option) (*Adapter, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("%w: google: Config.Model is required", types.ErrInvalidConfig)
	}
	model, apiKey := string(cfg.Model), cfg.APIKey
	a := &Adapter{model: model, backend: backend{kind: genai.BackendGeminiAPI}}
	for _, o := range opts {
		o(a)
	}
	if a.backend.kind == genai.BackendVertexAI && (a.backend.project == "" || a.backend.location == "") {
		return nil, errVertexTarget
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	// Fail at construction, not mid-stream: an unsupported server tool comes
	// back from Gemini as an opaque 400 on the first request that uses it.
	if err := types.ValidateServerTools(catalog.MustLookup(providerName, model), a.serverTools); err != nil {
		return nil, fmt.Errorf("google: %w", err)
	}
	client, err := a.backend.newClient(ctx, apiKey)
	if err != nil {
		return nil, err
	}
	a.client = client
	return a, nil
}

// Validate checks controls against the selected model, including after WithModel.
func (a *Adapter) Validate() error {
	caps := a.Capabilities()
	o := a.EffectiveOptions()
	if a.thinking != nil && !o.HasReasoning() {
		return caps.OptionError("reasoning", "an explicit thinking configuration needs a budget or level")
	}
	if err := caps.ValidateOptions(o); err != nil {
		return err
	}
	if len(a.safety) > 0 {
		if err := caps.Require(types.CapSafetySettings); err != nil {
			return err
		}
	}
	if err := types.ValidateServerTools(caps, a.serverTools); err != nil {
		return caps.OptionError("server_tools", err.Error())
	}
	return nil
}

// EffectiveOptions implements types.OptionsReporter: the generation config,
// thinking configuration and tool choice as request options.
func (a *Adapter) EffectiveOptions() types.RequestOptions {
	toFloat := func(p *float32) *float64 {
		if p == nil {
			return nil
		}
		v := float64(*p)
		return &v
	}
	o := types.RequestOptions{Temperature: toFloat(a.generation.Temperature),
		TopP: toFloat(a.generation.TopP), TopK: toFloat(a.generation.TopK), StopSequences: a.generation.StopSequences,
		FrequencyPenalty: toFloat(a.generation.FrequencyPenalty), PresencePenalty: toFloat(a.generation.PresencePenalty),
		ToolChoice: a.toolChoice, DialLayers: a.dials, DialPolicy: a.dialPolicy}
	if a.generation.Seed != nil {
		n := int64(*a.generation.Seed)
		o.Seed = &n
	}
	if a.generation.MaxOutputTokens != 0 {
		n := int64(a.generation.MaxOutputTokens)
		o.MaxOutputTokens = &n
	}
	if a.thinking != nil {
		if a.thinking.ThinkingBudget != nil {
			n := int64(*a.thinking.ThinkingBudget)
			o.ReasoningBudget = &n
		}
		if a.thinking.ThinkingLevel != "" {
			level := strings.ToLower(string(a.thinking.ThinkingLevel))
			o.ReasoningEffort = &level
		}
	}
	return o.Clone()
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return providerName }

// Model implements types.ModelProvider.
func (a *Adapter) Model() string { return a.model }

// WithTarget implements types.TargetSwitcher: a model target returns a copy
// of the adapter targeting that model, sharing the underlying client.
func (a *Adapter) WithTarget(t types.Target) (types.Provider, error) {
	m, err := types.TargetModel(t, a.Name())
	if err != nil {
		return nil, err
	}
	c := *a
	c.model = string(m)
	return &c, nil
}

// Generate sends a single-turn user prompt with no tools and returns the
// response text. It is the simple generation seam used by eval judges, HyDE,
// context compression, and KG extraction.
func (a *Adapter) Generate(ctx context.Context, prompt string) (string, error) {
	return generate.Text(ctx, a, prompt)
}

// Stream implements types.Provider. A request may carry a schema, options,
// or both.
func (a *Adapter) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	contents, config, err := a.prepare(req.Messages, req.Tools, req.Schema, req.Options)
	if err != nil {
		return nil, err
	}
	return a.chatStream(ctx, contents, config)
}

// SupportsSchema implements types.StructuredOutputProvider.
func (a *Adapter) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (a *Adapter) SupportsOptions() bool { return true }

// prepare validates a request and builds its contents and config. The
// stream and batch entry points share it, so they cannot drift apart.
//
// Each option set in opts overrides the adapter's configured value for this
// call only; unset options keep the configured ones. Options the model does
// not declare, and options Gemini has no field for (parallel tool control,
// a reasoning toggle), fail before any network I/O with an error matching
// types.ErrInvalidModelConfig, as does a part the request cannot carry
// (types.ErrModalityUnsupported, types.ErrMediaUnavailable). With a context
// cache bound, the tool configuration belongs to the cached resource, so a
// per-call tool choice that changes it is rejected.
func (a *Adapter) prepare(messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema, opts *types.RequestOptions) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	c := a
	var o types.RequestOptions
	if opts != nil {
		o = *opts
		var err error
		if c, err = a.withRequestOptions(o.Raw()); err != nil {
			return nil, nil, err
		}
	}
	c, err := c.compileDials(o, tools, schema != nil)
	if err != nil {
		return nil, nil, err
	}
	if err := c.Capabilities().ValidateRequest(tools, schema != nil); err != nil {
		return nil, nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	if err := c.checkToolChoice(tools); err != nil {
		return nil, nil, err
	}
	contents, config, err := c.cachedRequest(messages, tools)
	if err != nil {
		return nil, nil, err
	}
	if schema != nil {
		config.ResponseMIMEType = string(types.MediaJSON)
		config.ResponseSchema = parameterSchemaToGemini(*schema)
	}
	return contents, config, nil
}

// buildRequest converts messages[from:] and tools and applies every
// configured knob. Tool call names are learned from all of messages.
func (a *Adapter) buildRequest(messages []types.Message, from int, tools []types.ToolDef) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	systemInst, contents, err := a.mapper(messages, from).contents(messages[from:])
	if err != nil {
		return nil, nil, err
	}
	config := &genai.GenerateContentConfig{}
	if systemInst != nil {
		config.SystemInstruction = systemInst
	}
	gTools := toGeminiTools(tools)
	gTools = append(gTools, a.serverToolDecls()...)
	if len(gTools) > 0 {
		config.Tools = gTools
	}
	if len(tools) > 0 {
		config.ToolConfig = a.toolConfig()
	}
	a.generation.apply(config)
	if a.thinking != nil {
		config.ThinkingConfig = a.thinking
	}
	if len(a.safety) > 0 {
		config.SafetySettings = a.safety
	}
	for _, m := range a.modalities {
		config.ResponseModalities = append(config.ResponseModalities, string(m))
	}
	config.SpeechConfig = a.speech
	return contents, config, nil
}

// chatStream runs the streaming generation goroutine.
//
// Parts are walked directly rather than read through resp.Text(), because
// Text() skips thought parts entirely. The response mapper turns them into
// part deltas; see responseMapper for how runs and indices are assigned.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Adapter) chatStream(ctx context.Context, contents []*genai.Content, config *genai.GenerateContentConfig) (<-chan types.Delta, error) {
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		r := newResponseMapper(func(d types.Delta) { out <- d })
		structured := config != nil && (config.ResponseSchema != nil || config.ResponseJsonSchema != nil)
		var finishReason, finishMessage string
		var outputTokens int
		// blocked holds the prompt feedback of a prompt Gemini refused. Such
		// a stream completes with no candidate and so no finishReason.
		var blocked *genai.GenerateContentResponsePromptFeedback

		streamCtx, sink := withRetryAfterSink(ctx)
		for resp, err := range a.client.Models.GenerateContentStream(streamCtx, a.model, contents, config) {
			if err != nil {
				// Open parts stay open: the consumer keeps partial text and
				// drops what a cut-off stream cannot complete.
				out <- types.ErrorDelta{Error: classifyWithHeader(a.model, err, !r.started, sink)}
				return
			}

			if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
				blocked = resp.PromptFeedback
			}
			if len(resp.Candidates) > 0 {
				cand := resp.Candidates[0]
				if cand.FinishReason != "" {
					finishReason, finishMessage = string(cand.FinishReason), cand.FinishMessage
				}
				if err := r.candidate(cand); err != nil {
					out <- types.ErrorDelta{Error: &types.ProviderError{Provider: providerName, Model: a.model,
						Kind: types.ErrorKindPermanent, Err: err}}
					return
				}
			}

			if resp.UsageMetadata != nil {
				outputTokens = int(resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount)
				ud := usageOf(resp)
				if len(resp.Candidates) > 0 && string(resp.Candidates[0].FinishReason) != "" {
					ud.FinishReasons = []string{string(resp.Candidates[0].FinishReason)}
				}
				out <- ud
			}
		}

		// Search parts and grounding citations come last: Gemini sends the
		// complete grounding metadata with the final chunk.
		r.finish()
		switch {
		case blocked != nil:
			r.refusal(blocked.BlockReasonMessage, string(blocked.BlockReason))
			out <- types.ErrorDelta{Error: promptBlockedError(a.model, blocked)}
		case finishReason == "":
			// Gemini sets finishReason on the last chunk; a stream that ends
			// without one was cut off.
			out <- types.ErrorDelta{Error: streamcheck.StreamError(providerName, a.model, streamcheck.ErrIncompleteStream, !r.started)}
		case types.IsContentFilterFinishReason(finishReason):
			r.refusal(finishMessage, finishReason)
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, a.model, finishReason)}
		case finishReason == string(genai.FinishReasonMalformedFunctionCall):
			out <- types.ErrorDelta{Error: &types.ProviderError{
				Provider: providerName, Model: a.model, Kind: types.ErrorKindPermanent,
				Err: errors.New("model produced a malformed function call"),
			}}
		case structured && types.IsTruncationFinishReason(finishReason):
			// Schema output cut at the token limit is incomplete JSON.
			out <- types.ErrorDelta{Error: streamcheck.Truncated(providerName, a.model, finishReason,
				outputTokens, int(config.MaxOutputTokens))}
		}
	}()

	return out, nil
}

// usageOf reads a response's cumulative token usage.
func usageOf(resp *genai.GenerateContentResponse) types.UsageDelta {
	u := resp.UsageMetadata
	if u == nil {
		return types.UsageDelta{}
	}
	return types.UsageDelta{Cumulative: true,
		PromptTokens:         int(u.PromptTokenCount),
		CachedPromptTokens:   int(u.CachedContentTokenCount),
		CompletionTokens:     int(u.CandidatesTokenCount + u.ThoughtsTokenCount),
		TotalTokens:          int(u.TotalTokenCount),
		ResponseModel:        resp.ModelVersion,
		ResponseID:           resp.ResponseID,
		PromptByModality:     modalityCounts(u.PromptTokensDetails),
		CompletionByModality: modalityCounts(u.CandidatesTokensDetails),
	}
}

// modalityCounts maps Gemini's per-modality token details. Unspecified
// modalities are left out; nil when nothing was reported.
func modalityCounts(details []*genai.ModalityTokenCount) map[types.Modality]int {
	var out map[types.Modality]int
	for _, d := range details {
		if d == nil || d.TokenCount == 0 {
			continue
		}
		var m types.Modality
		switch d.Modality {
		case genai.MediaModalityText:
			m = types.ModalityText
		case genai.MediaModalityImage:
			m = types.ModalityImage
		case genai.MediaModalityAudio:
			m = types.ModalityAudio
		case genai.MediaModalityVideo:
			m = types.ModalityVideo
		case genai.MediaModalityDocument:
			m = types.ModalityDocument
		default:
			continue
		}
		if out == nil {
			out = map[types.Modality]int{}
		}
		out[m] += int(d.TokenCount)
	}
	return out
}

// promptBlockedError reports a prompt Gemini's safety system refused. It is a
// content-filter error, not a dropped stream, so retry does not resend it.
func promptBlockedError(model string, fb *genai.GenerateContentResponsePromptFeedback) *types.ProviderError {
	msg := "prompt blocked: " + string(fb.BlockReason)
	if fb.BlockReasonMessage != "" {
		msg += ": " + fb.BlockReasonMessage
	}
	return &types.ProviderError{Provider: providerName, Model: model, Kind: types.ErrorKindContentFilter, Err: errors.New(msg)}
}

// classifyGoogleError maps an error that ended a stream to a ProviderError. An
// API error is classified by status code and message, an ErrorInfo reason
// that names a bad credential makes it ErrorKindAuth, and a RetryInfo detail
// sets RetryAfter. Anything else is a transport failure, transient only while
// no output has reached the consumer.
func classifyGoogleError(model string, err error, beforeOutput bool) *types.ProviderError {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		var ptr *genai.APIError
		if errors.As(err, &ptr) && ptr != nil {
			apiErr = *ptr
		} else {
			return streamcheck.StreamError(providerName, model, err, beforeOutput)
		}
	}
	pe := streamcheck.HTTPError(providerName, model, apiErr.Code, nil, apiErr.Status+" "+apiErr.Message, err)
	if credentialRejected(apiErr.Details) {
		// Gemini reports a bad API key as 400 INVALID_ARGUMENT; the reason,
		// not the status, says it is an authentication failure.
		pe.Kind = types.ErrorKindAuth
	}
	if pe.Kind.Transient() {
		pe.RetryAfter = retryDelay(apiErr.Details)
	}
	return pe
}

// credentialReasons are the google.rpc.ErrorInfo reasons that mean the
// credential itself was rejected.
var credentialReasons = map[string]bool{
	"API_KEY_INVALID":      true,
	"API_KEY_EXPIRED":      true,
	"ACCESS_TOKEN_EXPIRED": true,
}

// credentialRejected reports whether the error details carry a
// google.rpc.ErrorInfo whose reason names a rejected credential.
func credentialRejected(details []map[string]any) bool {
	for _, d := range details {
		if t, _ := d["@type"].(string); !strings.HasSuffix(t, "google.rpc.ErrorInfo") {
			continue
		}
		if reason, _ := d["reason"].(string); credentialReasons[reason] {
			return true
		}
	}
	return false
}

// retryDelay reads google.rpc.RetryInfo.retryDelay ("7s", "1.5s") from the
// error details Gemini attaches to quota and overload errors.
func retryDelay(details []map[string]any) time.Duration {
	for _, d := range details {
		if t, _ := d["@type"].(string); !strings.HasSuffix(t, "google.rpc.RetryInfo") {
			continue
		}
		raw, _ := d["retryDelay"].(string)
		if delay, err := time.ParseDuration(raw); err == nil && delay > 0 {
			return delay
		}
	}
	return 0
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	return catalog.MustLookup(providerName, a.model)
}

// Catalog endpoints of the two Google backends.
const (
	endpointGemini = "google-gemini"
	endpointVertex = "google-vertex"
)

// Offering implements types.OfferingReporter: the model's offering on the
// backend this adapter targets. Vertex AI reads gs:// URIs, which the
// Gemini API does not.
func (a *Adapter) Offering() types.Offering {
	endpoint := endpointGemini
	if a.backend.kind == genai.BackendVertexAI {
		endpoint = endpointVertex
	}
	if o, ok := catalog.LookupOffering(endpoint, providerName, a.model); ok {
		return o
	}
	if caps := a.Capabilities(); caps.Offering != nil {
		return caps.Offering.Clone()
	}
	return types.OfferingFromCapabilities(a.Capabilities())
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
	if p.Nullable {
		nullable := true
		s.Nullable = &nullable
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
	case "null":
		return genai.TypeNULL
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
