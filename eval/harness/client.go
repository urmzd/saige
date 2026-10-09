// Package harness runs multi-turn live eval corpora against a chat model,
// either an OpenAI-compatible chat completions API or any saige
// [types.Provider].
//
// The harness is built on four abstractions:
//   - [Client]: a metered chat client with retries and usage capture
//   - [Script]: one scripted multi-turn eval case (system prompts plus ordered turns)
//   - [Flow]: a strategy for driving a script through the model
//   - [Runner]: runs flows over scripts and writes per-script metrics
//
// Two flows ship with the harness: [BaseFlow] (full conversational
// regeneration) and [StatelessFlow] (each edit re-sends only the current
// artifact plus the instruction). Downstream projects add protocol-specific
// flows by implementing [Flow] and, when needed, replace the assembled
// metrics document via [Runner.Assemble].
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// Message is a single chat message sent to the model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenAICompatible is the provider name of a [Client] that talks raw HTTP to
// an OpenAI-compatible /chat/completions endpoint.
const OpenAICompatible = "openai-compatible"

// DefaultAPIBase is the base URL [NewClient] uses when none is given.
const DefaultAPIBase = "https://api.openai.com/v1"

// DefaultModel is the model [NewClient] uses when none is given.
const DefaultModel = "gpt-4o-mini"

// maxAttempts bounds the HTTP retry loop of [Client.Chat].
const maxAttempts = 6

// Client is a metered chat client. It has two transports:
//
//   - HTTP (the default): requests go to APIBase + "/chat/completions" with
//     APIKey as a bearer token. Construct it with [NewClient].
//   - Provider: when Provider is set, requests go through that saige
//     provider instead, so the provider's own adapter, retry, fallback and
//     routing decorators apply. Construct it with [NewProviderClient].
//
// The zero value is not usable.
type Client struct {
	HTTPClient  *http.Client
	APIBase     string
	APIKey      string
	Model       string
	Temperature *float64
	Seed        *int64

	// Provider, when set, serves every Chat call. APIBase, APIKey and
	// HTTPClient are then unused, and retries are left to the provider
	// stack (for example a retry decorator), so Chat makes one attempt.
	// Temperature and Seed are sent as per-request options and need a
	// provider that implements [types.OptionsProvider]; leave them nil to
	// use the provider's configured sampling.
	Provider types.Provider
}

// NewProviderClient builds a Client that sends every request through p. Model
// is taken from p when it reports one. Temperature and Seed are left nil, so
// the provider's configured sampling applies.
func NewProviderClient(p types.Provider) *Client {
	return &Client{Provider: p, Model: types.ProviderModel(p)}
}

// ProviderName names the transport for metrics and error messages: the
// provider's name on the provider transport, [OpenAICompatible] otherwise.
func (c *Client) ProviderName() string {
	if c.Provider != nil {
		return types.ProviderName(c.Provider)
	}
	return OpenAICompatible
}

// ChatResult carries the model text plus token usage for one chat call.
type ChatResult struct {
	Text              string
	InputTokens       uint64
	OutputTokens      uint64
	CachedInputTokens uint64
	Retried           bool

	// retryAfter carries the server's requested wait from a retryable
	// response to the retry loop.
	retryAfter time.Duration
}

// Add accumulates next into r for a turn that took several calls, such as a
// validate-and-repair loop: token counts are summed, Retried is true when
// either call retried, and Text becomes next.Text, the latest answer.
func (r ChatResult) Add(next ChatResult) ChatResult {
	return ChatResult{
		Text:              next.Text,
		InputTokens:       r.InputTokens + next.InputTokens,
		OutputTokens:      r.OutputTokens + next.OutputTokens,
		CachedInputTokens: r.CachedInputTokens + next.CachedInputTokens,
		Retried:           r.Retried || next.Retried,
	}
}

// jsonSchemaFormat is the response_format type for strict JSON schema output.
const jsonSchemaFormat = "json_schema"

// chatOptions collects per-call options applied by [ChatOption] values.
type chatOptions struct {
	responseFormat map[string]any
	schema         map[string]any
}

// ChatOption customizes a single [Client.Chat] call.
type ChatOption func(*chatOptions)

// WithJSONSchema constrains the response to a strict JSON schema via the
// response_format json_schema mechanism. The schema map follows the JSON
// Schema subset accepted by OpenAI-compatible providers. On the provider
// transport the schema is sent through [types.StructuredOutputProvider], and
// a provider without it fails the call instead of answering unconstrained.
func WithJSONSchema(name string, schema map[string]any) ChatOption {
	return func(o *chatOptions) {
		o.schema = schema
		o.responseFormat = map[string]any{
			"type": jsonSchemaFormat,
			jsonSchemaFormat: map[string]any{
				"name":   name,
				"strict": true,
				"schema": schema,
			},
		}
	}
}

// NewClient builds an HTTP Client with deterministic defaults: seed 42,
// temperature 0, a 10 minute request timeout, [DefaultAPIBase] as the base
// URL and [DefaultModel] as the model. Temperature is omitted when the model
// catalog says the model rejects it (see [AcceptsTemperature]).
func NewClient(apiBase, apiKey, model string) *Client {
	apiBase = strings.TrimRight(apiBase, "/")
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if model == "" {
		model = DefaultModel
	}
	var temperature *float64
	if AcceptsTemperature(model) {
		t := 0.0
		temperature = &t
	}
	seed := int64(42)
	return &Client{
		HTTPClient:  &http.Client{Timeout: 10 * time.Minute},
		APIBase:     apiBase,
		APIKey:      apiKey,
		Model:       model,
		Temperature: temperature,
		Seed:        &seed,
	}
}

// AcceptsTemperature reports whether an OpenAI-style model accepts an
// explicit temperature with its default reasoning settings, according to
// the model catalog. Reasoning models such as o3 do not; models that accept
// temperature only with reasoning turned off, such as gpt-5.1, do not either
// unless reasoning is off by default. A model the catalog does not know
// resolves to the provider baseline, which accepts temperature.
func AcceptsTemperature(model string) bool {
	caps, _ := catalog.Lookup(providerOpenAI, model)
	t := 0.0
	return caps.ValidateOptions(types.RequestOptions{Temperature: &t}) == nil
}

// Chat sends one chat completion request.
//
// On the HTTP transport, failures are classified with the shared provider
// error taxonomy: rate limits, unavailable or overloaded servers, request
// timeouts and transient transport errors are retried up to 6 attempts
// with exponential backoff (1s doubling to 16s). When the server sends
// Retry-After, that wait is used instead, capped at [MaxRetryAfter].
// ChatResult.Retried reports whether any retry happened. A returned error
// is a *[types.ProviderError] for HTTP failures, so [types.KindOf] and
// errors.Is with the kind sentinels work on it.
//
// On the provider transport, Chat makes one attempt and returns the
// provider's error unchanged.
func (c *Client) Chat(ctx context.Context, messages []Message, opts ...ChatOption) (ChatResult, error) {
	var options chatOptions
	for _, opt := range opts {
		opt(&options)
	}
	if c.Provider != nil {
		return c.chatProvider(ctx, messages, options)
	}
	if c.APIKey == "" {
		return ChatResult{}, fmt.Errorf("missing API key for %s", c.APIBase)
	}

	body := map[string]any{
		"model":    c.Model,
		"messages": messages,
	}
	if c.Temperature != nil {
		body["temperature"] = *c.Temperature
	}
	if c.Seed != nil {
		body["seed"] = *c.Seed
	}
	if options.responseFormat != nil {
		body["response_format"] = options.responseFormat
	}

	data, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, err
	}

	var retried bool
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, retry, err := c.doChat(ctx, data)
		if err == nil {
			result.Retried = retried
			return result, nil
		}
		if attempt == maxAttempts || !retry {
			return ChatResult{}, err
		}
		retried = true
		wait := time.Duration(1<<min(attempt-1, 4)) * time.Second
		if result.retryAfter > 0 {
			wait = min(result.retryAfter, MaxRetryAfter)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ChatResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	return ChatResult{}, fmt.Errorf("unreachable retry state")
}

// chatProvider serves one Chat call through c.Provider.
func (c *Client) chatProvider(ctx context.Context, messages []Message, options chatOptions) (ChatResult, error) {
	msgs, err := toProviderMessages(messages)
	if err != nil {
		return ChatResult{}, err
	}
	var reqOpts *types.RequestOptions
	if c.Temperature != nil || c.Seed != nil {
		reqOpts = &types.RequestOptions{Temperature: c.Temperature, Seed: c.Seed}
	}

	var stream <-chan types.Delta
	switch {
	case options.schema != nil && reqOpts != nil:
		return ChatResult{}, fmt.Errorf("%w: a JSON schema cannot be combined with temperature or seed on the provider transport; configure sampling on the provider", types.ErrInvalidModelConfig)
	case options.schema != nil:
		sp, ok := c.Provider.(types.StructuredOutputProvider)
		if !ok {
			return ChatResult{}, fmt.Errorf("%w: provider %s does not support JSON schema output", types.ErrInvalidModelConfig, c.ProviderName())
		}
		schema, err := toParameterSchema(options.schema)
		if err != nil {
			return ChatResult{}, err
		}
		stream, err = sp.ChatStreamWithSchema(ctx, msgs, nil, schema)
		if err != nil {
			return ChatResult{}, err
		}
	case reqOpts != nil:
		op, ok := c.Provider.(types.OptionsProvider)
		if !ok {
			return ChatResult{}, fmt.Errorf("%w: provider %s does not accept per-request temperature or seed", types.ErrInvalidModelConfig, c.ProviderName())
		}
		stream, err = op.ChatStreamWithOptions(ctx, msgs, nil, *reqOpts)
		if err != nil {
			return ChatResult{}, err
		}
	default:
		stream, err = c.Provider.ChatStream(ctx, msgs, nil)
		if err != nil {
			return ChatResult{}, err
		}
	}
	return collectStream(stream)
}

// collectStream drains a provider stream into a ChatResult. The stream is
// always read to the end so the producer never blocks; the first error wins.
func collectStream(stream <-chan types.Delta) (ChatResult, error) {
	var text strings.Builder
	var usage types.UsageDelta
	var streamErr error
	for d := range stream {
		switch v := d.(type) {
		case types.TextContentDelta:
			text.WriteString(v.Content)
		case types.UsageDelta:
			usage = usage.Merge(v)
		case types.ErrorDelta:
			if streamErr == nil {
				streamErr = v.Error
			}
		}
	}
	if streamErr != nil {
		return ChatResult{}, streamErr
	}
	return ChatResult{
		Text:              text.String(),
		InputTokens:       uint64(max(usage.PromptTokens, 0)),
		OutputTokens:      uint64(max(usage.CompletionTokens, 0)),
		CachedInputTokens: uint64(max(usage.CachedPromptTokens, 0)),
	}, nil
}

// toProviderMessages maps role/content messages onto saige messages.
func toProviderMessages(messages []Message) ([]types.Message, error) {
	out := make([]types.Message, 0, len(messages))
	for i, m := range messages {
		switch m.Role {
		case roleSystem:
			out = append(out, types.NewSystemMessage(m.Content))
		case roleUser:
			out = append(out, types.NewUserMessage(m.Content))
		case roleAssistant:
			out = append(out, types.NewAssistantMessage(m.Content))
		default:
			return nil, fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
	}
	return out, nil
}

// toParameterSchema converts a JSON Schema map into the provider schema type.
func toParameterSchema(schema map[string]any) (*types.ParameterSchema, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode JSON schema: %w", err)
	}
	var out types.ParameterSchema
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("convert JSON schema: %w", err)
	}
	return &out, nil
}

// MaxRetryAfter caps how long [Client.Chat] honors a server's Retry-After.
const MaxRetryAfter = 60 * time.Second

// maxErrorBody caps how much of an error response body is embedded in an
// error message.
const maxErrorBody = 2048

// apiError builds the classified error for a non-2xx response, with the
// body cut to maxErrorBody bytes.
func (c *Client) apiError(resp *http.Response, body []byte) *types.ProviderError {
	text := string(body)
	if len(text) > maxErrorBody {
		text = Truncate(text, maxErrorBody) + "...(truncated)"
	}
	kind, retryAfter := types.ClassifyHTTPError(resp.StatusCode, resp.Header, text)
	return &types.ProviderError{
		Provider:   OpenAICompatible,
		Model:      c.Model,
		Kind:       kind,
		Code:       resp.StatusCode,
		Err:        errors.New(text),
		RetryAfter: min(retryAfter, MaxRetryAfter),
	}
}

// transportError wraps a failure to send a request or read its response in
// a ProviderError and reports whether it is transient.
func (c *Client) transportError(err error) (*types.ProviderError, bool) {
	kind, transient := types.ClassifyTransportError(err)
	if !transient {
		kind = types.ErrorKindPermanent
	}
	return &types.ProviderError{Provider: OpenAICompatible, Model: c.Model, Kind: kind, Err: err}, transient
}

// doChat makes one HTTP attempt. retry reports whether the failure is
// transient under the shared error taxonomy.
func (c *Client) doChat(ctx context.Context, data []byte) (result ChatResult, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return ChatResult{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ChatResult{}, false, ctx.Err()
		}
		perr, transient := c.transportError(err)
		return ChatResult{}, transient, perr
	}
	defer func() { _ = resp.Body.Close() }()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return ChatResult{}, false, ctx.Err()
		}
		perr, transient := c.transportError(err)
		return ChatResult{}, transient, perr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		perr := c.apiError(resp, respData)
		return ChatResult{retryAfter: perr.RetryAfter}, perr.Kind.Transient(), perr
	}

	var decoded chatResponse
	if err := json.Unmarshal(respData, &decoded); err != nil {
		return ChatResult{}, false, err
	}
	if len(decoded.Choices) == 0 {
		return ChatResult{}, false, fmt.Errorf("chat completion returned no choices")
	}

	return ChatResult{
		Text:              decoded.Choices[0].Message.Content,
		InputTokens:       decoded.Usage.PromptTokens,
		OutputTokens:      decoded.Usage.CompletionTokens,
		CachedInputTokens: decoded.Usage.PromptTokensDetails.CachedTokens,
	}, false, nil
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        uint64 `json:"prompt_tokens"`
		CompletionTokens    uint64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens uint64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}
