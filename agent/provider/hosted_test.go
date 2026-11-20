package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestBuildHostedVendor checks that a vendor the catalog serves through an
// OpenAI-compatible endpoint is built on the OpenAI adapter under its own
// name, with the endpoint's key variable and the model's own capabilities.
func TestBuildHostedVendor(t *testing.T) {
	const reply = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	var auth, model string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		auth, model = r.Header.Get("Authorization"), string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	keys := env(map[string]string{"FIREWORKS_API_KEY": "fw-key", "OPENAI_API_KEY": "openai-key"})

	for _, cfg := range []Config{
		{Provider: "fireworks", Model: "kimi-k3"},
		{Model: "fireworks/kimi-k3"},
		{Model: "kimi-k3"},
	} {
		cfg.BaseURL, cfg.Getenv = server.URL, keys
		cfg.Options = types.RequestOptions{ReasoningEffort: ptr("none")}
		p, err := Build(context.Background(), cfg)
		if err != nil {
			t.Fatalf("%+v: %v", cfg, err)
		}
		if got := p.(types.NamedProvider).Name(); got != "fireworks" {
			t.Errorf("name = %q, want fireworks", got)
		}
		caps := p.(types.CapabilityReporter).Capabilities()
		if !caps.Known || caps.Provider != "fireworks" || caps.ContextWindow != 1048576 {
			t.Errorf("capabilities = %s/%s known=%v window=%d", caps.Provider, caps.Model, caps.Known, caps.ContextWindow)
		}
		ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
		if err != nil {
			t.Fatal(err)
		}
		for d := range ch {
			if e, ok := d.(types.ErrorDelta); ok {
				t.Fatal(e.Error)
			}
		}
		if auth != "Bearer fw-key" {
			t.Errorf("authorization = %q, want the Fireworks key", auth)
		}
		if !strings.Contains(model, `"model":"kimi-k3"`) || !strings.Contains(model, `"reasoning_effort":"none"`) {
			t.Errorf("request body = %s", model)
		}
	}
}

// TestBuildHostedVendorErrors checks the failures a hosted vendor adds: a
// missing key names the endpoint's variable, an option the model does not
// take is rejected, and a vendor no endpoint serves is unknown.
func TestBuildHostedVendorErrors(t *testing.T) {
	none := env(nil)
	_, err := Build(context.Background(), Config{Provider: "fireworks", Model: "kimi-k3", Getenv: none})
	if !types.IsAuth(err) || !strings.Contains(err.Error(), "FIREWORKS_API_KEY") {
		t.Errorf("missing key: %v", err)
	}
	var pe *types.ProviderError
	if !errors.As(err, &pe) || pe.Provider != "fireworks" {
		t.Errorf("missing key names provider %+v", pe)
	}
	// GLM-5.3 cannot turn thinking off.
	_, err = Build(context.Background(), Config{Provider: "fireworks", Model: "glm-5p3", APIKey: "k",
		Options: types.RequestOptions{ReasoningEffort: ptr("none")}, Getenv: none})
	if err == nil {
		t.Error("reasoning effort none on glm-5p3: want an error")
	}
	// The OpenAI adapter has no top_k, so it is rejected, never dropped.
	_, err = Build(context.Background(), Config{Provider: "fireworks", Model: "kimi-k3", APIKey: "k",
		Options: types.RequestOptions{TopK: ptr(40.0)}, Getenv: none})
	if err == nil {
		t.Error("top_k on fireworks: want an error")
	}
	if _, err := Build(context.Background(), Config{Provider: "nowhere", Model: "m", APIKey: "k", Getenv: none}); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("unknown vendor: %v", err)
	}
}
