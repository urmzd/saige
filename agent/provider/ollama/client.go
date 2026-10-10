package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// Client is an HTTP client for the Ollama API.
type Client struct {
	Host           string
	Model          string
	EmbeddingModel string
	HTTP           *http.Client
	// Logger receives a debug line per call. Nil, the default, logs
	// nothing: a library must not write to stderr unasked, and an
	// interactive host's screen would be corrupted by it. Set it, for
	// example with WithLogger(log.Default()), to trace calls.
	Logger *log.Logger

	// ChatOptions is sent as the `options` object on every chat request. It is
	// how callers set num_ctx, temperature, num_predict, and the rest. Nil
	// sends no options object at all, so the daemon's defaults apply.
	//
	// num_ctx is worth setting explicitly: ollama defaults to a small context
	// window and silently truncates anything longer, which turns an oversized
	// prompt into a confidently wrong answer rather than an error.
	ChatOptions any

	// Think toggles the thinking phase on reasoning models for chat requests.
	// Nil leaves the model's default in place.
	Think *bool

	// StreamIdleTimeout ends a chat stream when no line arrives for this
	// long. It bounds a stalled connection without capping how long a healthy
	// stream may run. Zero uses DefaultStreamIdleTimeout; a negative value
	// disables the check.
	StreamIdleTimeout time.Duration
}

const (
	// DefaultResponseHeaderTimeout bounds the wait for response headers, which
	// covers loading the model into memory before the first token.
	DefaultResponseHeaderTimeout = 5 * time.Minute
	// DefaultStreamIdleTimeout bounds the gap between two streamed lines.
	DefaultStreamIdleTimeout = 2 * time.Minute

	// maxLineBytes caps one NDJSON line. A single chunk can carry a whole
	// tool call, so the 64KB scanner default is too small.
	maxLineBytes = 8 << 20
	// maxErrorBodyBytes caps how much of an error response is read.
	maxErrorBodyBytes = 8 << 10
)

// ErrStreamIdle reports a stream that stopped sending lines for longer than
// StreamIdleTimeout. It wraps context.DeadlineExceeded, so it classifies as a
// transient timeout.
var ErrStreamIdle = fmt.Errorf("ollama stream idle timeout: %w", context.DeadlineExceeded)

// StatusError is returned for a non-200 response. Body holds at most the
// first 8KB of the response, trimmed.
type StatusError struct {
	Code   int
	Body   string
	Header http.Header
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("ollama returned %d", e.Code)
	}
	return fmt.Sprintf("ollama returned %d: %s", e.Code, e.Body)
}

// statusError reads a bounded error body and closes it.
func statusError(resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
	return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body)), Header: resp.Header}
}

// Option configures a Client.
type Option func(*Client)

// WithChatOptions sets the `options` object sent on chat requests. Pass an
// Options value, or any struct or map that marshals to the shape ollama
// expects.
func WithChatOptions(opts any) Option {
	return func(c *Client) { c.ChatOptions = opts }
}

// WithThink enables or disables the thinking phase on reasoning models.
//
// Disabling it matters for schema-constrained output: the format grammar
// applies to everything the model emits, thinking included, so a reasoning
// model asked for JSON can spend its whole budget producing grammar-shaped
// reasoning and return empty content.
func WithThink(think bool) Option {
	return func(c *Client) { c.Think = &think }
}

// WithStreamIdleTimeout sets Client.StreamIdleTimeout.
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(c *Client) { c.StreamIdleTimeout = d }
}

// WithLogger sets Client.Logger, which traces every call. Without it the
// client logs nothing.
func WithLogger(l *log.Logger) Option {
	return func(c *Client) { c.Logger = l }
}

// logf writes one debug line when a Logger is set.
func (c *Client) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}

// WithHTTPClient replaces the underlying HTTP client, for callers that need a
// custom transport or timeout. Avoid http.Client.Timeout for streaming: it
// bounds the whole response body, so it cuts off long but healthy streams.
// StreamIdleTimeout bounds a stalled stream instead.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.HTTP = h }
}

// Options is the subset of ollama generation parameters callers most often
// set. Fields left at zero are omitted, so the daemon's default applies.
type Options struct {
	// NumCtx is the context window in tokens.
	NumCtx int `json:"num_ctx,omitempty"`
	// NumPredict caps generated tokens. -1 means unlimited.
	NumPredict int `json:"num_predict,omitempty"`
	// Temperature controls sampling randomness.
	Temperature float64 `json:"temperature,omitempty"`
	// TopP is nucleus sampling.
	TopP float64 `json:"top_p,omitempty"`
	// Seed makes sampling reproducible when set.
	Seed int `json:"seed,omitempty"`
	// Stop are sequences that end generation.
	Stop []string `json:"stop,omitempty"`
}

// DefaultHost is the address of a local Ollama daemon.
const DefaultHost = "http://localhost:11434"

// Config names the daemon and models a client or adapter uses.
type Config struct {
	// Host is the daemon's base URL. Empty means DefaultHost.
	Host string
	// Model is the generation model.
	Model types.ModelID
	// EmbeddingModel is the embedding model.
	EmbeddingModel types.ModelID
	// Client, for New, is a client built with NewClient (for example with
	// client options); it takes the place of Host and the models.
	Client *Client
}

// NewClient creates an Ollama client. A Host that is not an http or https
// URL is an error wrapping types.ErrInvalidConfig.
func NewClient(cfg Config, opts ...Option) (*Client, error) {
	host := cfg.Host
	if host == "" {
		host = DefaultHost
	}
	if u, err := url.Parse(host); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: ollama: host %q is not an http(s) URL", types.ErrInvalidConfig, host)
	}
	c := &Client{
		Host:           host,
		Model:          string(cfg.Model),
		EmbeddingModel: string(cfg.EmbeddingModel),
		HTTP:           defaultHTTPClient(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// defaultHTTPClient bounds the wait for response headers but not the body, so
// a stream may run as long as it keeps producing lines.
func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
}

// Generate sends a non-streaming generate request.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.GenerateWithModel(ctx, prompt, c.Model, nil, nil)
}

// GenerateWithModel sends a non-streaming generate request with a specific model.
func (c *Client) GenerateWithModel(ctx context.Context, prompt, model string, format, options any) (string, error) {
	c.logf("[ollama] generate model=%s prompt_len=%d", model, len(prompt))

	req := GenerateRequest{
		Model:   model,
		Prompt:  prompt,
		Stream:  false,
		Format:  format,
		Options: options,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal generate request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Host+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama generate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		statusErr := statusError(resp)
		c.logf("[ollama] generate failed: %d %s", statusErr.Code, statusErr.Body)
		return "", statusErr
	}

	var result GenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode ollama response: %w", err)
	}
	if types.IsTruncationFinishReason(result.DoneReason) {
		// A cut-off reply parsed as complete is worse than an error: callers
		// such as extractors would read half a JSON document as the answer.
		return "", &types.ResponseTruncatedError{FinishReason: types.FinishReasonMaxTokens, OutputTokens: result.EvalCount}
	}
	response := result.Response
	if response == "" && result.Thinking != "" {
		response = result.Thinking
	}
	c.logf("[ollama] generate done, response_len=%d", len(response))
	return response, nil
}

// GenerateStream sends a streaming generate request.
func (c *Client) GenerateStream(ctx context.Context, prompt string) (<-chan string, error) {
	ch := make(chan string, 64)

	req := GenerateRequest{
		Model:  c.Model,
		Prompt: prompt,
		Stream: true,
	}

	body, err := json.Marshal(req)
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Host+"/api/generate", bytes.NewReader(body))
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("ollama generate stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		close(ch)
		return ch, statusError(resp)
	}

	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)

		// The channel carries text only, so a read error or a server error
		// line can only be logged here. Use ChatStream when errors matter.
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		defer func() {
			if err := scanner.Err(); err != nil {
				c.logf("[ollama] generate stream read failed: %v", err)
			}
		}()
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var chunk GenerateResponse
			if err := json.Unmarshal(line, &chunk); err != nil {
				c.logf("[ollama] generate stream: malformed line: %v", err)
				return
			}
			if chunk.Error != "" {
				c.logf("[ollama] generate stream error: %s", chunk.Error)
				return
			}
			if chunk.Response != "" {
				select {
				case ch <- chunk.Response:
				case <-ctx.Done():
					return
				}
			}
			if chunk.Done {
				return
			}
		}
	}()

	return ch, nil
}

// Embed generates embeddings for the given text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	c.logf("[ollama] embed text_len=%d", len(text))

	req := EmbedRequest{
		Model: c.EmbeddingModel,
		Input: text,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Failures are classified like chat errors, so a retrying caller sees a
	// 429, a 503 or a dropped connection as transient.
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, classifyOllamaError(c.EmbeddingModel, fmt.Errorf("ollama embed: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyOllamaError(c.EmbeddingModel, fmt.Errorf("ollama embed: %w", statusError(resp)))
	}

	var result EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}

	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}
	return result.Embeddings[0], nil
}

// ChatStream sends a streaming chat request.
func (c *Client) ChatStream(ctx context.Context, messages []ChatMessage, tools []Tool) (<-chan ChatChunk, error) {
	req := ChatRequest{
		Model:    c.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Options:  c.ChatOptions,
		Think:    c.Think,
	}
	return c.doChatStream(ctx, req)
}

// ChatStreamWithFormat sends a streaming chat request with a format constraint.
//
// When the caller has not set Think explicitly, thinking is disabled for these
// requests: the format grammar constrains every token the model emits, so a
// reasoning model left to think will produce grammar-shaped reasoning and
// return no usable content. Set WithThink(true) to override.
func (c *Client) ChatStreamWithFormat(ctx context.Context, messages []ChatMessage, tools []Tool, format any) (<-chan ChatChunk, error) {
	think := c.Think
	if think == nil && format != nil && !catalog.MustLookup("ollama", c.Model).ReasoningRequired {
		off := false
		think = &off
	}
	req := ChatRequest{
		Model:    c.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Format:   format,
		Options:  c.ChatOptions,
		Think:    think,
	}
	return c.doChatStream(ctx, req)
}

// doChatStream executes the chat streaming HTTP request.
//
// The returned channel ends with a chunk whose Done is true on success. On
// failure the last chunk carries Error (a server error line) or Err (a read
// failure, a malformed line, or an idle timeout). A channel that closes with
// neither was cut off, most often by caller cancellation.
func (c *Client) doChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	c.logf("[ollama] chat_stream model=%s msgs=%d tools=%d", c.Model, len(req.Messages), len(req.Tools))

	ch := make(chan ChatChunk, 64)

	body, err := json.Marshal(req)
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("marshal chat request: %w", err)
	}

	// reqCtx lets the idle timer abort a stalled body read.
	reqCtx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", c.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		cancel()
		close(ch)
		return ch, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		cancel()
		close(ch)
		return ch, fmt.Errorf("ollama chat_stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// The body carries the actionable part ("model not found, try pulling
		// it"); dropping it leaves the caller with a bare status code.
		cancel()
		close(ch)
		statusErr := statusError(resp)
		c.logf("[ollama] chat_stream failed: %d %s", statusErr.Code, statusErr.Body)
		return ch, statusErr
	}

	idle := c.StreamIdleTimeout
	if idle == 0 {
		idle = DefaultStreamIdleTimeout
	}
	var idleExpired atomic.Bool
	var timer *time.Timer
	if idle > 0 {
		timer = time.AfterFunc(idle, func() {
			idleExpired.Store(true)
			cancel()
		})
	}

	go func() {
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)
		if timer != nil {
			defer timer.Stop()
		}

		send := func(chunk ChatChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		for scanner.Scan() {
			// The timer measures the server's silence, not the consumer's
			// pace: it is paused while a chunk waits to be delivered.
			if timer != nil {
				timer.Stop()
			}
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				if timer != nil {
					timer.Reset(idle)
				}
				continue
			}
			var chunk ChatChunk
			if err := json.Unmarshal(line, &chunk); err != nil {
				send(ChatChunk{Err: fmt.Errorf("ollama chat_stream: malformed line: %w", err)})
				return
			}
			if !send(chunk) || chunk.Done || chunk.Error != "" {
				return
			}
			if timer != nil {
				timer.Reset(idle)
			}
		}
		err := scanner.Err()
		if idleExpired.Load() {
			err = ErrStreamIdle
		}
		if err != nil && ctx.Err() == nil {
			if errors.Is(err, bufio.ErrTooLong) {
				err = fmt.Errorf("ollama chat_stream: line exceeds %d bytes: %w", maxLineBytes, err)
			} else {
				err = fmt.Errorf("ollama chat_stream: %w", err)
			}
			send(ChatChunk{Err: err})
		}
	}()

	return ch, nil
}
