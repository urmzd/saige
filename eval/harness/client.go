// Package harness runs multi-turn live eval corpora against any
// OpenAI-compatible chat completions API.
//
// The harness is built on four abstractions:
//   - [Client]: a metered /chat/completions client with retries and usage capture
//   - [Experiment]: one multi-turn eval case (system prompts plus ordered turns)
//   - [Flow]: a strategy for driving an experiment through the model
//   - [Runner]: runs flows over experiments and writes per-experiment metrics
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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is a single chat message sent to the model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client is a metered client for an OpenAI-compatible /chat/completions
// endpoint. Construct it with [NewClient]; the zero value is not usable.
type Client struct {
	HTTPClient  *http.Client
	APIBase     string
	APIKey      string
	Model       string
	Temperature *float64
	Seed        *int64
}

// ChatResult carries the model text plus token usage for one chat call.
type ChatResult struct {
	Text              string
	InputTokens       uint64
	OutputTokens      uint64
	CachedInputTokens uint64
	Retried           bool
}

// jsonSchemaFormat is the response_format type for strict JSON schema output.
const jsonSchemaFormat = "json_schema"

// chatOptions collects per-call options applied by [ChatOption] values.
type chatOptions struct {
	responseFormat map[string]any
}

// ChatOption customizes a single [Client.Chat] call.
type ChatOption func(*chatOptions)

// WithJSONSchema constrains the response to a strict JSON schema via the
// response_format json_schema mechanism. The schema map follows the JSON
// Schema subset accepted by OpenAI-compatible providers.
func WithJSONSchema(name string, schema map[string]any) ChatOption {
	return func(o *chatOptions) {
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

// NewClient builds a Client with deterministic defaults: seed 42, temperature
// 0 (omitted for models that reject it, currently o1/o3/o4/gpt-5 prefixes),
// a 10 minute request timeout, and https://api.openai.com/v1 as the default
// base URL. Model defaults to gpt-4o-mini.
func NewClient(apiBase, apiKey, model string) *Client {
	apiBase = strings.TrimRight(apiBase, "/")
	if apiBase == "" {
		apiBase = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	var temperature *float64
	lower := strings.ToLower(model)
	if !strings.HasPrefix(lower, "o1") &&
		!strings.HasPrefix(lower, "o3") &&
		!strings.HasPrefix(lower, "o4") &&
		!strings.HasPrefix(lower, "gpt-5") {
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

// Chat sends one chat completion request. Transport errors, HTTP 429, and
// HTTP 5xx responses are retried up to 6 attempts with exponential backoff;
// ChatResult.Retried reports whether any retry happened.
func (c *Client) Chat(ctx context.Context, messages []Message, opts ...ChatOption) (ChatResult, error) {
	if c.APIKey == "" {
		return ChatResult{}, fmt.Errorf("missing API key")
	}

	var options chatOptions
	for _, opt := range opts {
		opt(&options)
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
	for attempt := 1; attempt <= 6; attempt++ {
		result, retry, err := c.doChat(ctx, data)
		if err == nil && !retry {
			result.Retried = retried
			return result, nil
		}
		if attempt == 6 || !retry {
			if err != nil {
				return ChatResult{}, err
			}
			return result, nil
		}
		retried = true
		select {
		case <-ctx.Done():
			return ChatResult{}, ctx.Err()
		case <-time.After(time.Duration(1<<min(attempt-1, 4)) * time.Second):
		}
	}
	return ChatResult{}, fmt.Errorf("unreachable retry state")
}

func (c *Client) doChat(ctx context.Context, data []byte) (ChatResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return ChatResult{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return ChatResult{}, true, err
	}
	defer func() { _ = resp.Body.Close() }()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResult{}, false, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return ChatResult{}, true, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respData))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResult{}, false, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respData))
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
