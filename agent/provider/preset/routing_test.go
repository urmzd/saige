package preset_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

// serve runs one request and returns the serving entry's model, or the
// request's error.
func serve(t *testing.T, s types.Provider) (string, error) {
	t.Helper()
	ch, err := s.ChatStream(context.Background(), []types.Message{types.NewUserMessage("hi")}, nil)
	if err != nil {
		return "", err
	}
	var out string
	var failed error
	for d := range ch {
		switch v := d.(type) {
		case types.TextContentDelta:
			out += v.Content
		case types.ErrorDelta:
			failed = v.Error
		}
	}
	return strings.TrimPrefix(out, "ok "), failed
}

func TestFailoverOnAuthIsOptIn(t *testing.T) {
	cat := overlay(t, `{"version":1,"presets":{
		"strict":{"retry":{"disable":true},"chain":[
			{"id":"a","provider":"openai","model":"gpt-6-luna"},
			{"id":"b","provider":"google","model":"gemini-3.1-flash-lite"}]},
		"lenient":{"extends":"strict","routing":{"failover_on_auth":true}}}}`)
	for _, tt := range []struct {
		name   string
		served string
	}{{"strict", ""}, {"lenient", "gemini-3.1-flash-lite"}} {
		rec := newRecorder()
		rec.scripts["gpt-6-luna"] = fails(types.ErrorKindAuth)
		b, err := preset.Build(context.Background(), cat, tt.name, nil, preset.Options{Getenv: everyone, Factory: rec.factory})
		if err != nil {
			t.Fatal(err)
		}
		got, err := serve(t, b.Session())
		if tt.served == "" {
			if !types.IsAuth(err) || got != "" {
				t.Fatalf("%s: an auth failure must end the request, got %q %v", tt.name, got, err)
			}
		} else if err != nil || got != tt.served {
			t.Fatalf("%s: served %q, err %v", tt.name, got, err)
		}
		_ = b.Close()
	}
}

func TestReprobeReturnsToPrimary(t *testing.T) {
	cat := overlay(t, `{"version":1,"presets":{
		"sticky":{"retry":{"disable":true},"chain":[
			{"id":"a","provider":"openai","model":"gpt-6-luna"},
			{"id":"b","provider":"google","model":"gemini-3.1-flash-lite"}]},
		"returns":{"extends":"sticky","routing":{"fail_threshold":1,"reprobe_after":1}}}}`)
	for _, tt := range []struct {
		name        string
		wantPrimary bool
	}{{"sticky", false}, {"returns", true}} {
		rec := newRecorder()
		// The primary fails once, then recovers.
		rec.scripts["gpt-6-luna"] = func(n int) (<-chan types.Delta, error) {
			if n == 0 {
				return nil, &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("overloaded")}
			}
			return text("ok gpt-6-luna"), nil
		}
		b, err := preset.Build(context.Background(), cat, tt.name, nil, preset.Options{Getenv: everyone, Factory: rec.factory})
		if err != nil {
			t.Fatal(err)
		}
		s := b.Session()
		var served []string
		for range 5 {
			got, err := serve(t, s)
			if err != nil {
				t.Fatal(err)
			}
			served = append(served, got)
		}
		back := served[len(served)-1] == "gpt-6-luna"
		if served[0] != "gemini-3.1-flash-lite" || back != tt.wantPrimary {
			t.Fatalf("%s: served %v, want return to primary %v", tt.name, served, tt.wantPrimary)
		}
		_ = b.Close()
	}
}

func TestRoutingThresholdsRejectSticky(t *testing.T) {
	layer, err := catalog.Load(strings.NewReader(`{"version":1,"presets":{"p":{"routing":{"policy":"sticky","reprobe_after":2},
		"chain":[{"provider":"openai","model":"gpt-6-luna"}]}}}`))
	if err == nil {
		_, err = catalog.Merge(catalog.Default(), layer)
	}
	if !errors.Is(err, catalog.ErrInvalidCatalog) || !strings.Contains(err.Error(), "routing.policy") {
		t.Fatalf("got %v", err)
	}
}

func TestOptionalOllamaDroppedWhenUnreachable(t *testing.T) {
	cat := overlay(t, `{"version":1,"presets":{"p":{"chain":[
		{"id":"cloud","provider":"openai","model":"gpt-6-luna","optional":true},
		{"id":"local","provider":"ollama","model":"qwen3","optional":true}]}}}`)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"0.0.0"}`))
	}))
	defer up.Close()
	noKeys := func(string) string { return "" }
	for _, tt := range []struct {
		name, host string
		wantErr    bool
	}{{"unreachable", down.URL, true}, {"reachable", up.URL, false}} {
		env := func(k string) string {
			if k == "OLLAMA_HOST" {
				return tt.host
			}
			return noKeys(k)
		}
		b, err := preset.Build(context.Background(), cat, "p", nil, preset.Options{Getenv: env, Factory: newRecorder().factory})
		if tt.wantErr {
			if err == nil || !strings.Contains(err.Error(), "every entry was dropped") {
				t.Fatalf("%s: got %v", tt.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if rp, _ := b.Resolved("p"); len(rp.Chain) != 1 || rp.Chain[0].ID != "local" {
			t.Fatalf("%s: chain %+v", tt.name, rp.Chain)
		}
		_ = b.Close()
	}
}

func TestAvailable(t *testing.T) {
	rp, err := catalog.Default().ResolveModel("anthropic", "claude-haiku-5-5")
	if err != nil {
		t.Fatal(err)
	}
	o := preset.Options{Getenv: func(string) string { return "" }}
	if err := preset.Available(context.Background(), rp.Chain[0], o); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("missing key: %v", err)
	}
	o.Getenv = everyone
	if err := preset.Available(context.Background(), rp.Chain[0], o); err != nil {
		t.Fatal(err)
	}
	local, err := catalog.Default().ResolveModel("ollama", "qwen3")
	if err != nil {
		t.Fatal(err)
	}
	probed := false
	o.Probe = func(_ context.Context, cfg provider.Config) error {
		probed = cfg.Provider == provider.Ollama
		return errors.New("down")
	}
	if err := preset.Available(context.Background(), local.Chain[0], o); err == nil || !probed {
		t.Fatalf("probe not consulted: %v", err)
	}
}

func TestRouteNamesTheAdapterNotItsDecorators(t *testing.T) {
	cat := overlay(t, chainDoc)
	b, err := preset.Build(context.Background(), cat, "p", nil, preset.Options{Getenv: everyone, Factory: newRecorder().factory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	routes, _ := drain(t)(b.Session().ChatStream(context.Background(), nil, nil))
	// Every entry is wrapped in a retry decorator, which names itself
	// "retry(openai)"; the route reports the vendor.
	if len(routes) == 0 || routes[0].Provider != "openai" || routes[0].Model != "gpt-4.1" {
		t.Fatalf("routes %+v", routes)
	}
}
