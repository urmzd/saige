package anthropic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
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

// errPausedTurn reports a server tool turn the API paused before its answer
// and the adapter could not continue.
var errPausedTurn = errors.New("response paused during server tool use before a final answer; the turn could not be continued")

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
	_ types.TargetSwitcher           = (*Adapter)(nil)
	_ types.CapabilityReporter       = (*Adapter)(nil)
	_ types.OptionsReporter          = (*Adapter)(nil)
)

// Adapter wraps the official Anthropic SDK client and implements types.Provider,
// types.NamedProvider and types.StructuredOutputProvider.
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
	endpoint      string // workspace that scopes Files API IDs
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
// one Stream call, hidden from that accounting, and stack under an outer
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

// Config names the account and model an adapter serves.
type Config struct {
	// APIKey authenticates every request.
	APIKey string
	// Model is the model requests go to. Required.
	Model types.ModelID
}

// New creates an Anthropic provider adapter using the official SDK. A
// missing model is an error wrapping types.ErrInvalidConfig. Option
// combinations are checked against the model by Validate and before every
// request.
func New(cfg Config, opts ...Option) (*Adapter, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("%w: anthropic: Config.Model is required", types.ErrInvalidConfig)
	}
	a := &Adapter{
		model:     anthropic.Model(cfg.Model),
		maxTokens: 4096,
	}
	for _, o := range opts {
		o(a)
	}
	if a.maxTokens <= 0 {
		return nil, fmt.Errorf("%w: anthropic: max tokens must be positive", types.ErrInvalidConfig)
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if a.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(a.baseURL))
	}
	clientOpts = append(clientOpts, option.WithMaxRetries(a.maxRetries))
	clientOpts = append(clientOpts, a.requestOpts...)
	a.client = anthropic.NewClient(clientOpts...)
	return a, nil
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
// nativeSchema reports whether schema output goes through
// output_config.format instead of a forced hidden tool. Models that reject a
// forced tool choice take the native path; every other model keeps the
// hidden tool.
func (a *Adapter) nativeSchema(caps types.ModelCapabilities) bool {
	return caps.RejectsForcedToolChoice && a.thinking == nil
}

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

// WithTarget implements types.TargetSwitcher: a model target returns a copy
// of the adapter targeting that model, sharing the underlying client.
func (a *Adapter) WithTarget(t types.Target) (types.Provider, error) {
	m, err := types.TargetModel(t, a.Name())
	if err != nil {
		return nil, err
	}
	c := *a
	c.model = anthropic.Model(m)
	return &c, nil
}

// Generate sends a single-turn user prompt with no tools and returns the
// response text. It is the simple generation seam used by eval judges, HyDE,
// context compression, and KG extraction.
func (a *Adapter) Generate(ctx context.Context, prompt string) (string, error) {
	return generate.Text(ctx, a, prompt)
}

// Stream implements types.Provider. A request may carry a schema, options,
// or both: each option set overrides the adapter's configured value for this
// call only. Every check, including the mapping of each part, runs before
// any network I/O, so a request the model cannot take fails with an error
// matching types.ErrInvalidModelConfig and a fallback can try another
// member.
func (a *Adapter) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	var opts types.RequestOptions
	if req.Options != nil {
		opts = *req.Options
	}
	c, params, structured, err := a.buildParams(req.Messages, req.Tools, req.Schema, opts)
	if err != nil {
		return nil, err
	}
	return c.consumeStream(ctx, params, structured), nil
}

// SupportsSchema implements types.StructuredOutputProvider.
func (a *Adapter) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (a *Adapter) SupportsOptions() bool { return true }

// buildParams validates one request and builds its Messages parameters. It
// returns the adapter with the request's options applied, and whether the
// schema rides on the forced hidden tool, whose input the stream reports as
// text. A schema goes out as native structured output
// (output_config.format) on models that reject a forced tool choice, and as
// the hidden tool on the others. The streaming and batch paths share it, so
// a batch request carries exactly what a streaming call would.
func (a *Adapter) buildParams(messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema, opts types.RequestOptions) (*Adapter, anthropic.MessageNewParams, bool, error) {
	var params anthropic.MessageNewParams
	c, err := a.withRequestOptions(opts.Raw())
	if err != nil {
		return nil, params, false, err
	}
	if c, err = c.compileDials(opts, tools, schema != nil); err != nil {
		return nil, params, false, err
	}
	caps := c.Capabilities()
	row := catalog.MustLookup("anthropic", string(c.model))
	native := schema != nil && c.nativeSchema(row)
	if schema != nil && !native {
		// Schema output forces a hidden tool, which the API rejects with a
		// manual thinking budget.
		if why := c.forcedToolBlocked(row); why != "" {
			return nil, params, false, schemacheck.Unsupported(c, "forced-tool schema output: "+why)
		}
	}
	if err := caps.ValidateRequest(tools, schema != nil); err != nil {
		return nil, params, false, err
	}
	if err := c.Validate(); err != nil {
		return nil, params, false, err
	}
	if schema != nil && !native {
		if err := caps.Require(types.CapTools, types.CapToolChoice); err != nil {
			return nil, params, false, err
		}
		if err := c.checkSchemaToolChoice(); err != nil {
			return nil, params, false, err
		}
	}
	if err := c.checkToolChoice(tools); err != nil {
		return nil, params, false, err
	}

	systemBlocks, msgs, err := c.toAnthropicParams(messages)
	if err != nil {
		return nil, params, false, err
	}
	aTools := append(toAnthropicTools(tools), c.serverToolParams()...)
	params = anthropic.MessageNewParams{Model: c.model, MaxTokens: c.maxTokens, Messages: msgs, System: systemBlocks}
	c.applyParams(&params)
	switch {
	case native:
		params.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: outputFormatSchema(*schema)}
	case schema != nil:
		// A hidden tool whose input schema is the response schema.
		props := make(map[string]any, len(schema.Properties))
		for k, v := range schema.Properties {
			props[k] = propertyToSchema(v)
		}
		aTools = append(aTools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        structuredToolName,
			Description: anthropic.String("Return the structured response"),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: schema.Required},
		}})
		params.ToolChoice = anthropic.ToolChoiceParamOfTool(structuredToolName)
	}
	if len(aTools) > 0 {
		params.Tools = aTools
	}
	c.applyToolChoice(&params, len(params.Tools) > 0)
	if err := c.applyPromptCache(&params); err != nil {
		return nil, params, false, err
	}
	return c, params, schema != nil && !native, nil
}

// Capabilities implements types.CapabilityReporter: it resolves the target
// model against the shared catalog so callers can check whether a flag (e.g.
// reasoning) is supported before building a request that would be rejected.
//
// Extended thinking rejects a request that ends with an assistant turn, so
// an adapter that thinks does not report assistant prefill. Schema output
// forces a tool call, except on models that reject forcing, where it uses
// output_config.format; an adapter with a manual thinking budget cannot force
// a tool and does not report structured output.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	caps := catalog.MustLookup("anthropic", string(a.model))
	if a.thinking != nil || a.reasoningEffort != nil || caps.ReasoningDefaultEnabled {
		caps = caps.Without(types.CapAssistantPrefill)
	}
	if !a.nativeSchema(caps) && a.forcedToolBlocked(caps) != "" {
		caps = caps.Without(types.CapStructuredOutput)
	}
	return caps
}

// ── Conversion helpers ──────────────────────────────────────────────

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

// outputFormatSchema builds the output_config.format schema. Structured
// outputs require every object to set additionalProperties to false, which a
// ParameterSchema cannot express, so it is added to each object here.
func outputFormatSchema(ps types.ParameterSchema) map[string]any {
	schema := map[string]any{"type": ps.Type}
	if len(ps.Required) > 0 {
		schema["required"] = ps.Required
	}
	props := make(map[string]any, len(ps.Properties))
	for k, v := range ps.Properties {
		props[k] = propertyToSchema(v)
	}
	schema["properties"] = props
	closeObjects(schema)
	return schema
}

// closeObjects sets additionalProperties to false on every object in node.
func closeObjects(node map[string]any) {
	object := node["type"] == "object"
	if union, ok := node["type"].([]string); ok {
		object = slices.Contains(union, "object")
	}
	if object {
		node["additionalProperties"] = false
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for _, child := range props {
			if m, ok := child.(map[string]any); ok {
				closeObjects(m)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		closeObjects(items)
	}
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
