package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Client is an HTTP client for the Ollama API.
type Client struct {
	Host           string
	Model          string
	EmbeddingModel string
	HTTP           *http.Client
	Logger         *log.Logger
}

// NewClient creates a new Ollama client.
func NewClient(host, model, embeddingModel string) *Client {
	return &Client{
		Host:           host,
		Model:          model,
		EmbeddingModel: embeddingModel,
		HTTP: &http.Client{
			Timeout: 300 * time.Second,
		},
		Logger: log.Default(),
	}
}

// Generate sends a non-streaming generate request.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.GenerateWithModel(ctx, prompt, c.Model, nil, nil)
}

// GenerateWithModel sends a non-streaming generate request with a specific model.
func (c *Client) GenerateWithModel(ctx context.Context, prompt, model string, format, options any) (string, error) {
	c.Logger.Printf("[ollama] generate model=%s prompt_len=%d", model, len(prompt))

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
		respBody, _ := io.ReadAll(resp.Body)
		c.Logger.Printf("[ollama] generate failed: %d %s", resp.StatusCode, string(respBody))
		return "", fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result GenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode ollama response: %w", err)
	}
	c.Logger.Printf("[ollama] generate done, response_len=%d", len(result.Response))
	return result.Response, nil
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
		_ = resp.Body.Close()
		close(ch)
		return ch, fmt.Errorf("ollama returned %d", resp.StatusCode)
	}

	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var chunk GenerateResponse
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
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
	c.Logger.Printf("[ollama] embed text_len=%d", len(text))

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

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed returned %d: %s", resp.StatusCode, string(respBody))
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
	}
	return c.doChatStream(ctx, req)
}

// ChatStreamWithFormat sends a streaming chat request with a format constraint.
func (c *Client) ChatStreamWithFormat(ctx context.Context, messages []ChatMessage, tools []Tool, format any) (<-chan ChatChunk, error) {
	req := ChatRequest{
		Model:    c.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Format:   format,
	}
	return c.doChatStream(ctx, req)
}

// doChatStream executes the chat streaming HTTP request.
func (c *Client) doChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	c.Logger.Printf("[ollama] chat_stream model=%s msgs=%d tools=%d", c.Model, len(req.Messages), len(req.Tools))

	ch := make(chan ChatChunk, 64)

	body, err := json.Marshal(req)
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("ollama chat_stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		close(ch)
		c.Logger.Printf("[ollama] chat_stream failed: %d", resp.StatusCode)
		return ch, fmt.Errorf("ollama returned %d", resp.StatusCode)
	}

	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var chunk ChatChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}
			done := chunk.Done
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return
			}
			if done {
				return
			}
		}
	}()

	return ch, nil
}
