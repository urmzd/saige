package native_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/turn/native"
)

// compatServer is a minimal OpenAI-compatible server: it records each chat
// completions request and streams back one short answer.
type compatServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []compatRequest
}

type compatRequest struct {
	path, auth string
	body       map[string]any
}

const compatStream = `data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hello from "},"finish_reason":null}]}

data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"content":"a compatible server"},"finish_reason":null}]}

data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}

data: [DONE]

`

func newCompatServer(t *testing.T) *compatServer {
	t.Helper()
	s := &compatServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.reqs = append(s.reqs, compatRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, compatStream)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *compatServer) only(t *testing.T) compatRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(s.reqs))
	}
	return s.reqs[0]
}

func openaiRevision(model string) native.Revision {
	return native.Revision{Name: "docs-bot", Version: "1.0.0", Spec: native.Spec{
		Name: "docs-bot", Provider: native.ProviderOpenAI, Model: model, SystemPrompt: "You document things.",
	}}
}

// The built-in provider reaches a compatible server through OPENAI_BASE_URL
// alone: a local server needs no key, and none is sent.
func TestOpenAIProviderRunsATurnAgainstACompatibleServer(t *testing.T) {
	srv := newCompatServer(t)
	t.Setenv(native.EnvOpenAIBaseURL, srv.URL+"/v1")
	t.Setenv(native.EnvOpenAIKey, "")

	r, err := native.New(openaiRevision("qwen3:8b"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, res := collect(t, r, native.Turn{Text: "say hello"})

	if res.Text != "hello from a compatible server" {
		t.Errorf("answer is %q", res.Text)
	}
	got := srv.only(t)
	if got.path != "/v1/chat/completions" {
		t.Errorf("request went to %q, want /v1/chat/completions", got.path)
	}
	if got.body["model"] != "qwen3:8b" {
		t.Errorf("request names model %v, want the spec's", got.body["model"])
	}
	if got.auth != "" {
		t.Errorf("a keyless provider sent Authorization %q", got.auth)
	}
	if res.Usage.PromptTokens != 9 || res.Usage.CompletionTokens != 5 {
		t.Errorf("usage is %+v, want the server's 9 in and 5 out", res.Usage)
	}
}

// A provider a host registers for one server is configured by its arguments
// only: the OpenAI key in the environment must never reach a third party.
func TestOpenAICompatibleNeverSendsTheEnvironmentKeyElsewhere(t *testing.T) {
	srv := newCompatServer(t)
	other := newCompatServer(t)
	t.Setenv(native.EnvOpenAIKey, "sk-openai-secret")
	t.Setenv(native.EnvOpenAIBaseURL, other.URL)

	rev := openaiRevision("llama")
	rev.Spec.Provider = "vllm"
	r, err := native.New(rev, native.WithOpenAICompatible(native.OpenAICompatible{Name: "vllm", BaseURL: srv.URL}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, r, native.Turn{Text: "hi"})

	got := srv.only(t)
	if got.auth != "" {
		t.Errorf("the environment's OpenAI key leaked to a compatible server: Authorization %q", got.auth)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.reqs) != 0 {
		t.Errorf("the provider followed %s to another server", native.EnvOpenAIBaseURL)
	}
}

// An endpoint with a key env var sends that variable's value as a bearer
// token, read when the agent is built.
func TestOpenAICompatibleSendsTheKeyFromItsEnvVar(t *testing.T) {
	srv := newCompatServer(t)
	t.Setenv("GATEWAY_KEY", "gw-key")
	rev := openaiRevision("m")
	rev.Spec.Provider = "gateway"
	r, err := native.New(rev, native.WithOpenAICompatible(native.OpenAICompatible{Name: "gateway", BaseURL: srv.URL, KeyEnv: "GATEWAY_KEY"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, r, native.Turn{Text: "hi"})
	if got := srv.only(t).auth; got != "Bearer gw-key" {
		t.Errorf("Authorization is %q, want the provider's key", got)
	}
}

// Pointed at nothing, the provider refuses to build rather than calling
// OpenAI without a key; with no model it refuses too.
func TestOpenAIProviderRefusesAnIncompleteConfiguration(t *testing.T) {
	tests := []struct {
		name, base, key, model string
	}{
		{name: "no key and no base url", model: "gpt-x"},
		{name: "no model", key: "sk", model: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(native.EnvOpenAIBaseURL, tt.base)
			t.Setenv(native.EnvOpenAIKey, tt.key)
			_, err := native.New(openaiRevision(tt.model))
			if !errors.Is(err, native.ErrInvalid) {
				t.Fatalf("New: %v, want ErrInvalid", err)
			}
		})
	}
}

// An endpoint that names a key variable needs it set: building without it
// would only move the failure to the first model call.
func TestOpenAICompatibleRefusesAKeyVariableThatIsUnset(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "")
	rev := openaiRevision("m")
	rev.Spec.Provider = "gateway"
	_, err := native.New(rev, native.WithOpenAICompatible(native.OpenAICompatible{Name: "gateway", BaseURL: "http://127.0.0.1:1/v1", KeyEnv: "GATEWAY_KEY"}))
	if !errors.Is(err, native.ErrInvalid) {
		t.Fatalf("New: %v, want ErrInvalid", err)
	}
}
