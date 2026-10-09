package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// RetryPolicy repeats HTTP requests to a remote server that were rejected as
// throttled or unavailable.
//
// Which requests qualify depends on whether repeating them is safe. A
// tools/call POST is not idempotent, and a 502 or 504 can arrive after the
// tool already ran, so:
//
//   - 429, and 503 with Retry-After, are retried for every method: the server
//     refused the request before doing anything.
//   - Other 502, 503 and 504 responses are retried only for methods that read
//     (initialize, ping, tools/list, resources/*, prompts/*) and for
//     tools/call when the tool is known to be idempotent.
//   - Transport errors are never retried here: the request may have been
//     delivered.
//
// The handshake policy (ServerSpec.ConnectRetry) also retries 500, because
// initialize has no side effects.
type RetryPolicy struct {
	// MaxAttempts counts the first attempt. Values below 2 disable retries.
	MaxAttempts int
	// InitialBackoff is the first delay; each later delay doubles, with full
	// jitter, up to MaxBackoff. Zero defaults to 200ms and 5s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// MaxTotalDelay caps the sum of all waits for one request, including
	// server-requested Retry-After delays. Zero defaults to 30s.
	MaxTotalDelay time.Duration
	// OnRetry observes each retry before its wait, for metrics and budgets.
	OnRetry func(RetryAttempt)
}

// RetryAttempt describes one retry about to happen.
type RetryAttempt struct {
	Server string
	// Method is the JSON-RPC method, and Tool the remote tool name for
	// tools/call.
	Method string
	Tool   string
	// Attempt is the number of the attempt about to be made, starting at 2.
	Attempt int
	Status  int
	Wait    time.Duration
}

// DefaultRetryPolicy returns three attempts with backoff from 200ms to 5s.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{MaxAttempts: 3, InitialBackoff: 200 * time.Millisecond, MaxBackoff: 5 * time.Second, MaxTotalDelay: 30 * time.Second}
}

func (p *RetryPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.MaxAttempts < 0 || p.InitialBackoff < 0 || p.MaxBackoff < 0 || p.MaxTotalDelay < 0 {
		return fmt.Errorf("retry policy values must not be negative")
	}
	return nil
}

func (p *RetryPolicy) backoff(retry int) time.Duration {
	initial, max := p.InitialBackoff, p.MaxBackoff
	if initial <= 0 {
		initial = 200 * time.Millisecond
	}
	if max <= 0 {
		max = 5 * time.Second
	}
	d := initial << (retry - 1)
	if d <= 0 || d > max {
		d = max
	}
	return time.Duration(rand.Int64N(int64(d) + 1)) //nolint:gosec // jitter needs no cryptographic randomness
}

func (p *RetryPolicy) totalCap() time.Duration {
	if p.MaxTotalDelay > 0 {
		return p.MaxTotalDelay
	}
	return 30 * time.Second
}

// readMethods are JSON-RPC methods that change nothing on the server.
var readMethods = map[string]bool{
	"initialize":               true,
	"ping":                     true,
	"tools/list":               true,
	"resources/list":           true,
	"resources/templates/list": true,
	"resources/read":           true,
	"prompts/list":             true,
	"prompts/get":              true,
	"completion/complete":      true,
}

// handshakeMethods are the requests that make up connecting.
var handshakeMethods = map[string]bool{
	"initialize":                true,
	"notifications/initialized": true,
}

// retryTransport applies RetryPolicy to JSON-RPC POSTs.
type retryTransport struct {
	base       http.RoundTripper
	server     string
	call       *RetryPolicy
	connect    *RetryPolicy
	idempotent func(remoteTool string) bool
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil {
		return rt.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	method, tool := peekRPC(body)
	handshake := handshakeMethods[method]
	policy := rt.call
	if handshake {
		policy = rt.connect
	}

	attempt := func() (*http.Response, error) {
		r := req.Clone(req.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		r.ContentLength = int64(len(body))
		return rt.base.RoundTrip(r)
	}
	if policy == nil || policy.MaxAttempts < 2 {
		return attempt()
	}

	var waited time.Duration
	for n := 1; ; n++ {
		resp, err := attempt()
		if err != nil {
			return nil, err
		}
		if n >= policy.MaxAttempts || !rt.retryable(resp, method, tool, handshake) {
			return resp, nil
		}
		wait := policy.backoff(n)
		if _, ok := resp.Header["Retry-After"]; ok {
			wait = types.RetryAfterFromHeader(resp.Header, time.Now())
		} else if _, ok := resp.Header["Retry-After-Ms"]; ok {
			wait = types.RetryAfterFromHeader(resp.Header, time.Now())
		}
		if waited+wait > policy.totalCap() {
			return resp, nil
		}
		// The response is discarded; drain it so the connection is reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if policy.OnRetry != nil {
			policy.OnRetry(RetryAttempt{Server: rt.server, Method: method, Tool: tool, Attempt: n + 1, Status: resp.StatusCode, Wait: wait})
		}
		waited += wait
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		}
	}
}

func (rt *retryTransport) retryable(resp *http.Response, method, tool string, handshake bool) bool {
	code := resp.StatusCode
	if code == http.StatusTooManyRequests {
		return true
	}
	_, hasRetryAfter := resp.Header["Retry-After"]
	if code == http.StatusServiceUnavailable && hasRetryAfter {
		return true
	}
	if handshake {
		return code >= 500 && code <= 504
	}
	switch code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if readMethods[method] {
			return true
		}
		return method == "tools/call" && tool != "" && rt.idempotent != nil && rt.idempotent(tool)
	}
	return false
}

// peekRPC extracts the JSON-RPC method and, for tools/call, the tool name. A
// body it cannot parse (a batch, say) yields empty strings, which only the
// unconditional 429 rule can retry.
func peekRPC(body []byte) (method, tool string) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return "", ""
	}
	if msg.Method == "tools/call" {
		return msg.Method, msg.Params.Name
	}
	return msg.Method, ""
}
