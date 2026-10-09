package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/auth"
)

// vertexTransport records each request and answers with a permission
// error, so no request leaves the process.
type vertexTransport struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (c *vertexTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req.Clone(context.Background()))
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`)), Request: req}, nil
}

func (c *vertexTransport) first(t *testing.T) *http.Request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatal("no request was sent")
	}
	return c.reqs[0]
}

type staticToken string

func (s staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: string(s), Type: "Bearer"}, nil
}

func testCredentials(token string) *auth.Credentials {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken(token)})
}

// TestEmbedderVertex checks that the embedder reaches Vertex AI with the
// project and location it was given and the credentials it was handed,
// instead of the hardcoded Gemini API.
func TestEmbedderVertex(t *testing.T) {
	rt := &vertexTransport{}
	e, err := NewEmbedder(context.Background(), "", "gemini-embedding-001",
		WithEmbedVertex("proj-1", "global"), WithEmbedHTTPClient(&http.Client{Transport: rt}),
		WithEmbedCredentials(testCredentials("tok-1")))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.Embed(context.Background(), []string{"hi"})
	req := rt.first(t)
	if want := "aiplatform.googleapis.com/v1beta1/projects/proj-1/locations/global/publishers/google/models/gemini-embedding-001"; !strings.Contains(req.URL.String(), want) {
		t.Fatalf("url = %s, want it to contain %s", req.URL, want)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("authorization = %q", got)
	}
}

// TestAdapterVertexCredentials checks that a Vertex adapter attaches the
// credentials it was given to its requests.
func TestAdapterVertexCredentials(t *testing.T) {
	rt := &vertexTransport{}
	a, err := NewAdapter(context.Background(), "", "gemini-3.1-flash-lite", WithVertex("proj-1", "us-central1"),
		WithHTTPClient(&http.Client{Transport: rt}), WithCredentials(testCredentials("tok-2")))
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := a.ChatStream(context.Background(), nil, nil); err == nil {
		for range ch {
		}
	}
	req := rt.first(t)
	if !strings.Contains(req.URL.Host, "us-central1-aiplatform.googleapis.com") {
		t.Fatalf("host = %s", req.URL.Host)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-2" {
		t.Fatalf("authorization = %q", got)
	}
}

// TestVertexDetectsDefaultCredentials checks that a Vertex client without an
// HTTP client or credentials looks up Application Default Credentials, and
// reports a missing lookup as an error at construction.
func TestVertexDetectsDefaultCredentials(t *testing.T) {
	saved := detectCredentials
	t.Cleanup(func() { detectCredentials = saved })

	calls := 0
	detectCredentials = func() (*auth.Credentials, error) { calls++; return testCredentials("adc"), nil }
	if _, err := NewEmbedder(context.Background(), "", "gemini-embedding-001", WithEmbedVertex("p", "global")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("credential lookups = %d, want 1", calls)
	}

	detectCredentials = func() (*auth.Credentials, error) { return nil, errors.New("no adc") }
	if _, err := NewAdapter(context.Background(), "", "gemini-3.1-flash-lite", WithVertex("p", "global")); err == nil ||
		!strings.Contains(err.Error(), "Application Default Credentials") {
		t.Fatalf("err = %v, want a missing-credentials error", err)
	}
	if _, err := NewEmbedder(context.Background(), "", "m", WithEmbedVertex("", "global")); !errors.Is(err, errVertexTarget) {
		t.Fatalf("err = %v, want the missing-project error", err)
	}
}
