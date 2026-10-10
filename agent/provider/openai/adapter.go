package openai

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"

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
	_ types.TargetSwitcher           = (*Adapter)(nil)
	_ types.CapabilityReporter       = (*Adapter)(nil)
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
	reasoningSummary *string
	audio            *audioOutput
	parallelTools    *bool
	toolChoice       *types.ToolChoice
	dials            []types.DialLayer
	dialPolicy       *types.DialPolicy
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

// WithPresencePenalty sets the presence penalty.
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

// Config names the account and model an adapter or embedder serves.
type Config struct {
	// APIKey authenticates every request.
	APIKey string
	// Model is the model requests go to. Required.
	Model types.ModelID
}

// New creates an OpenAI Chat Completions adapter using the official SDK. A
// missing model is an error wrapping types.ErrInvalidConfig. Option
// combinations are checked against the model by Validate and before every
// request.
func New(cfg Config, opts ...Option) (*Adapter, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("%w: openai: Config.Model is required", types.ErrInvalidConfig)
	}
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return &Adapter{
		client: openai.NewClient(c.clientOptions(cfg.APIKey, new(int))...),
		model:  openai.ChatModel(cfg.Model),
		params: c.params,
	}, nil
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
		ToolChoice: a.params.toolChoice, DialLayers: a.params.dials, DialPolicy: a.params.dialPolicy,
	}.Clone()
}

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return providerName }

// Model implements types.ModelProvider.
func (a *Adapter) Model() string { return string(a.model) }

// WithTarget implements types.TargetSwitcher: a model target returns a copy
// of the adapter targeting that model, sharing the underlying client.
func (a *Adapter) WithTarget(t types.Target) (types.Provider, error) {
	m, err := types.TargetModel(t, a.Name())
	if err != nil {
		return nil, err
	}
	c := *a
	c.model = openai.ChatModel(m)
	return &c, nil
}

// Generate sends a single-turn user prompt with no tools and returns the
// response text. It is the simple generation seam used by eval judges, HyDE,
// context compression, and KG extraction.
func (a *Adapter) Generate(ctx context.Context, prompt string) (string, error) {
	return generate.Text(ctx, a, prompt)
}

// Stream implements types.Provider. A request may carry a schema, options,
// or both. Every part is mapped natively or the request fails before it is
// sent (see toOpenAIMessages).
func (a *Adapter) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	c, params, err := a.chatParams(req)
	if err != nil {
		return nil, err
	}
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	return c.consumeStream(stream, req.Schema != nil), nil
}

// SupportsSchema implements types.StructuredOutputProvider.
func (a *Adapter) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (a *Adapter) SupportsOptions() bool { return true }

// forRequest returns a copy of the adapter with req's options and its dials
// compiled for surface applied. Unset options keep the configured values.
func (a *Adapter) forRequest(req types.Request, surface string) (*Adapter, error) {
	c := a
	var o types.RequestOptions
	if req.Options != nil {
		o = *req.Options
		var err error
		if c, err = a.withRequestOptions(o.Raw()); err != nil {
			return nil, err
		}
	}
	return c.compileDials(o, req.Tools, req.Schema != nil, surface)
}

// chatParams validates req and encodes it as a Chat Completions request,
// without the streaming fields. It sends nothing. The returned adapter
// carries the request's options.
func (a *Adapter) chatParams(req types.Request) (*Adapter, openai.ChatCompletionNewParams, error) {
	var params openai.ChatCompletionNewParams
	c, err := a.forRequest(req, types.SurfaceChat)
	if err != nil {
		return nil, params, err
	}
	caps := c.Capabilities()
	if err := c.Validate(); err != nil {
		return nil, params, err
	}
	if err := c.checkAudioOutput(); err != nil {
		return nil, params, err
	}
	if c.params.reasoningSummary != nil {
		return nil, params, caps.OptionError("reasoning_summary", "Chat Completions returns no reasoning; use NewResponsesAdapter")
	}
	if err := caps.ValidateRequest(req.Tools, req.Schema != nil); err != nil {
		return nil, params, err
	}
	if err := c.checkToolChoice(req.Tools); err != nil {
		return nil, params, err
	}
	toolsEffortNone, err := c.checkChatTools(req.Tools)
	if err != nil {
		return nil, params, err
	}
	msgs, err := toOpenAIMessages(req.Messages)
	if err != nil {
		return nil, params, wrapPartError(caps, err)
	}
	params = openai.ChatCompletionNewParams{Model: c.model, Messages: msgs}
	c.applyParams(&params)
	if err := c.applyPromptCache(&params); err != nil {
		return nil, params, err
	}
	if oTools := toOpenAITools(req.Tools); len(oTools) > 0 {
		params.Tools = oTools
		c.applyToolChoice(&params)
		if toolsEffortNone {
			params.ReasoningEffort = shared.ReasoningEffort(reasoningNone)
		}
	}
	if req.Schema != nil {
		schemaMap, strict := responseSchema(*req.Schema)
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "response",
					Schema: schemaMap,
					Strict: openai.Bool(strict),
				},
			},
		}
	}
	c.applyAudioOutput(&params)
	return c, params, nil
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	return catalog.MustLookup(providerName, string(a.model))
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
		// The SDK's Error() is a bare status line, which leaves nothing to
		// act on. Add the API's own message, but never the request URL or
		// raw body, which can carry secrets.
		if apiErr.Message != "" {
			detail := apiErr.Message
			if apiErr.Code != "" {
				detail = apiErr.Code + ": " + detail
			}
			err = fmt.Errorf("%w: %s", err, detail)
		}
		return streamcheck.HTTPError(providerName, model, apiErr.StatusCode, header, msg, err)
	}
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		return streamcheck.EventError(providerName, model, string(streamErr.Event.Data), err)
	}
	return streamcheck.StreamError(providerName, model, err, beforeOutput)
}
