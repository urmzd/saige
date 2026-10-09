package google

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// The genai SDK turns an error response into an APIError built from the body
// alone, so the Retry-After header a 429 or 503 carries never reaches the
// caller. headerTransport records it on a sink carried by the request context,
// and the adapter reads the sink when it classifies the error.

// retryAfterSink holds the delay the most recent error response asked for.
type retryAfterSink struct {
	mu    sync.Mutex
	delay time.Duration
}

func (s *retryAfterSink) set(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

func (s *retryAfterSink) get() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delay
}

type retryAfterSinkKey struct{}

// withRetryAfterSink returns a context whose requests record Retry-After on
// the returned sink.
func withRetryAfterSink(ctx context.Context) (context.Context, *retryAfterSink) {
	s := &retryAfterSink{}
	return context.WithValue(ctx, retryAfterSinkKey{}, s), s
}

// headerTransport records the server-requested delay of an error response.
type headerTransport struct {
	base http.RoundTripper
	now  func() time.Time
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 400 {
		return resp, err
	}
	if s, ok := req.Context().Value(retryAfterSinkKey{}).(*retryAfterSink); ok {
		if d := types.RetryAfterFromHeader(resp.Header, t.now()); d > 0 {
			s.set(d)
		}
	}
	return resp, err
}

// withHeaderTransport returns a copy of h (or of the default client) whose
// transport records Retry-After. The caller's client is not modified.
func withHeaderTransport(h *http.Client) *http.Client {
	var c http.Client
	if h != nil {
		c = *h
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = headerTransport{base: base, now: time.Now}
	return &c
}

// classifyWithHeader is classifyGoogleError plus the Retry-After header the
// failed response carried. The longer of the header and the RetryInfo detail
// wins, so a retry never comes back sooner than either asked.
func classifyWithHeader(model string, err error, beforeOutput bool, sink *retryAfterSink) *types.ProviderError {
	pe := classifyGoogleError(model, err, beforeOutput)
	if d := sink.get(); d > pe.RetryAfter && pe.Kind.Transient() {
		pe.RetryAfter = d
	}
	return pe
}
