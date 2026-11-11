// Package httpjson is the JSON-over-HTTP client the connectors' real API
// implementations share.
//
// It exists because three connectors need the same six lines — marshal, set
// headers, do, check the status, read the body, unmarshal — and because the two
// details that are easy to get wrong belong in one place: a non-2xx response must
// carry the provider's error body into the error (a Slack or GitHub failure is
// only diagnosable from its body), and the body must be drained and closed so the
// connection returns to the pool.
//
// This is not an SDK and not a general HTTP abstraction: no retries, no rate
// limiting, no pagination. Each connector's API surface here is a handful of
// endpoints, and every one of them is called from a path that already has a
// policy for failure.
package httpjson

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

// DefaultTimeout bounds one API call. A sink is called from a delivery loop that
// has other runs to serve, so a hung provider must not hold it.
const DefaultTimeout = 15 * time.Second

// maxErrorBody bounds how much of a failure response is carried into an error.
// Enough for a provider's JSON error object, not enough to log an HTML error
// page.
const maxErrorBody = 4 << 10

// Client calls one API base with a fixed set of headers.
type Client struct {
	// HTTP performs the requests. Nil means a client with DefaultTimeout.
	HTTP *http.Client
	// Base is the API root, without a trailing slash.
	Base string
	// Header is sent on every request: authorization, accept, api version.
	Header http.Header
}

// Error is a non-2xx response.
type Error struct {
	Status int
	Method string
	URL    string
	// Body is the response body, truncated. Providers put the reason here and
	// nowhere else.
	Body string
}

func (e *Error) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s %s: %s", e.Method, e.URL, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.URL, e.Status, http.StatusText(e.Status), e.Body)
}

// Retryable reports whether repeating the request could plausibly succeed: a
// rate limit, or the provider having a bad moment. A 4xx other than 429 is our
// bug or our credential, and repeating it only spends quota.
func (e *Error) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Do performs one call. in is marshalled as the request body when non-nil, and
// out is unmarshalled from the response body when non-nil.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("httpjson: marshal %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}

	url := c.Base + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("httpjson: build %s %s: %w", method, url, err)
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if in != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("httpjson: %s %s: %w", method, url, err)
	}
	defer func() {
		// Drain before closing: an undrained body cannot be reused, and these
		// connectors call the same host repeatedly.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &Error{
			Status: resp.StatusCode,
			Method: method,
			URL:    url,
			Body:   strings.TrimSpace(string(snippet)),
		}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("httpjson: decode %s %s: %w", method, url, err)
	}
	return nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}
