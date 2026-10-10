package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

func TestResolveVertex(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    *Vertex
		env  map[string]string
		want *Vertex
	}{
		{name: "gemini api by default", env: map[string]string{EnvCloudProject: "p"}},
		{name: "env flag selects vertex", env: map[string]string{EnvUseVertex: "true", EnvCloudProject: "p"},
			want: &Vertex{Project: "p", Location: "global"}},
		{name: "env flag 1", env: map[string]string{EnvUseVertex: "1", EnvCloudProject: "p", EnvCloudLocation: "us-central1"},
			want: &Vertex{Project: "p", Location: "us-central1"}},
		{name: "env flag false", env: map[string]string{EnvUseVertex: "false", EnvCloudProject: "p"}},
		{name: "explicit wins over env", v: &Vertex{Project: "x", Location: "eu"}, env: map[string]string{EnvCloudProject: "p", EnvCloudLocation: "us"},
			want: &Vertex{Project: "x", Location: "eu"}},
		{name: "empty config fills from env", v: &Vertex{}, env: map[string]string{EnvCloudProject: "p"},
			want: &Vertex{Project: "p", Location: "global"}},
		{name: "no project stays empty", v: &Vertex{}, want: &Vertex{Location: "global"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveVertex(tc.v, env(tc.env))
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("ResolveVertex = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBuildVertexRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   Config
		check func(error) bool
	}{
		{"no project", Config{Provider: Google, Model: "gemini-3.1-flash-lite", Vertex: &Vertex{}, Getenv: env(nil)},
			func(err error) bool { return types.IsAuth(err) && strings.Contains(err.Error(), EnvCloudProject) }},
		{"api key with vertex", Config{Provider: Google, Model: "gemini-3.1-flash-lite", APIKey: "k", Vertex: &Vertex{Project: "p"}, Getenv: env(nil)},
			func(err error) bool { return errors.Is(err, types.ErrInvalidModelConfig) }},
		{"vertex on another provider", Config{Provider: OpenAI, Model: "gpt-6-luna", APIKey: "k", Vertex: &Vertex{Project: "p"}, Getenv: env(nil)},
			func(err error) bool { return errors.Is(err, types.ErrInvalidModelConfig) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(context.Background(), tc.cfg); !tc.check(err) {
				t.Fatalf("Build = %v", err)
			}
		})
	}
}

// recordingTransport records request URLs and fails every request, so a
// test sees where the adapter sends a call without any network.
type recordingTransport struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, req.URL.String())
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`)), Request: req}, nil
}

// TestBuildVertexTarget checks that GOOGLE_GENAI_USE_VERTEXAI routes a Google
// model to Vertex AI with the project and location from the environment, and
// that no Gemini API key is needed. The caller's HTTP client is trusted to
// authenticate, so no credential lookup runs.
func TestBuildVertexTarget(t *testing.T) {
	rt := &recordingTransport{}
	p, err := Build(context.Background(), Config{Provider: Google, Model: "gemini-3.1-flash-lite",
		HTTPClient: &http.Client{Transport: rt},
		Getenv:     env(map[string]string{EnvUseVertex: "true", EnvCloudProject: "proj-1", EnvCloudLocation: "us-central1"})})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
	if err == nil {
		for range ch {
		}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.urls) == 0 {
		t.Fatal("no request was sent")
	}
	want := "us-central1-aiplatform.googleapis.com/v1beta1/projects/proj-1/locations/us-central1/publishers/google/models/gemini-3.1-flash-lite"
	if !strings.Contains(rt.urls[0], want) {
		t.Fatalf("url = %s, want it to contain %s", rt.urls[0], want)
	}
}

// TestBuildRoutesResponsesOnlyModels checks that a model whose tools need
// the Responses API is built on it, and other OpenAI models stay on Chat
// Completions.
func TestBuildRoutesResponsesOnlyModels(t *testing.T) {
	for model, wantResponses := range map[string]bool{"gpt-6.1-sol": true, "gpt-6-astra": true, "gpt-6-luna": false} {
		p, err := Build(context.Background(), Config{Provider: OpenAI, Model: types.ModelID(model), APIKey: "k", Getenv: env(nil)})
		if err != nil {
			t.Fatal(err)
		}
		_, isResponses := wrapper.As[*openai.ResponsesAdapter](p)
		if isResponses != wantResponses {
			t.Errorf("%s: adapter %T, want Responses=%v", model, p, wantResponses)
		}
	}
}
