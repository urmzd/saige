package catalog_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/google"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
)

type requestTransport func(*http.Request) (*http.Response, error)

func (f requestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEveryAdapterRejectsUnsupportedRequestBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()
	client := &http.Client{Transport: requestTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	for _, provider := range []string{"openai", "anthropic", "google", "ollama"} {
		for _, missing := range []types.Capability{types.CapStreaming, types.CapTools, types.CapStructuredOutput} {
			t.Run(provider+"/"+string(missing), func(t *testing.T) {
				model := "request-validation-" + string(missing)
				caps := types.ModelCapabilities{}.With(types.CapStreaming, types.CapTools, types.CapStructuredOutput, types.CapMaxOutputTokens).Without(missing)
				catalog.Register(catalog.Entry{Provider: provider, Prefix: model, Caps: caps})
				var p types.Provider
				switch provider {
				case "openai":
					p = openai.NewAdapter("local-test", model, openai.WithBaseURL(server.URL))
				case "anthropic":
					p = anthropic.NewAdapter("local-test", model, anthropic.WithBaseURL(server.URL))
				case "google":
					a, err := google.NewAdapter(context.Background(), "local-test", model, google.WithHTTPClient(client))
					if err != nil {
						t.Fatal(err)
					}
					p = a
				case "ollama":
					p = ollama.NewAdapter(ollama.NewClient(server.URL, model, ""))
				}
				var tools []types.ToolDef
				if missing == types.CapTools {
					tools = []types.ToolDef{{Name: "test", Parameters: types.ParameterSchema{Type: "object"}}}
				}
				for _, schema := range []bool{false, true} {
					if missing == types.CapStructuredOutput && !schema {
						continue
					}
					var ch <-chan types.Delta
					var err error
					if schema {
						ch, err = p.(types.StructuredOutputProvider).ChatStreamWithSchema(context.Background(), nil, tools, &types.ParameterSchema{Type: "object"})
					} else {
						ch, err = p.ChatStream(context.Background(), nil, tools)
					}
					if ch != nil {
						for range ch {
						}
					}
					if !errors.Is(err, types.ErrInvalidModelConfig) || types.IsTransient(err) || !strings.Contains(err.Error(), string(missing)) {
						t.Fatalf("schema=%v error=%v, want permanent %s rejection", schema, err, missing)
					}
				}
			})
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("sent %d invalid HTTP requests", got)
	}
}
