// Package searxng provides an HTTP client for SearXNG metasearch instances.
package searxng

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Result is a single search result from SearXNG.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// Client talks to a SearXNG instance over HTTP.
type Client struct {
	http    *http.Client
	baseURL string
}

// New creates a SearXNG client for the given base URL.
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Search queries SearXNG and returns deduplicated results (up to 8).
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/search", nil)
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}

	q := req.URL.Query()
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("engines", "google,bing")
	req.URL.RawQuery = q.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned %d", resp.StatusCode)
	}

	var body searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("parse searxng response: %w", err)
	}

	var results []Result
	seenURLs := make(map[string]bool)

	for _, r := range body.Results {
		if r.URL == "" || seenURLs[r.URL] {
			continue
		}
		seenURLs[r.URL] = true

		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
		})

		if len(results) >= 8 {
			break
		}
	}

	log.Printf("searxng returned %d results for %q", len(results), query)
	return results, nil
}
