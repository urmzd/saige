package router

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// optsProvider accepts options and reports configured ones.
type optsProvider struct {
	provider
	configured types.RequestOptions
	got        *[]types.RequestOptions
}

func (p optsProvider) ChatStreamWithOptions(_ context.Context, _ []types.Message, _ []types.ToolDef, o types.RequestOptions) (<-chan types.Delta, error) {
	if p.got != nil {
		*p.got = append(*p.got, o)
	}
	return p.call()
}
func (p optsProvider) EffectiveOptions() types.RequestOptions { return p.configured }

func ok(text string) func() (<-chan types.Delta, error) {
	return func() (<-chan types.Delta, error) { return deltas(types.TextContentDelta{Content: text}), nil }
}

func collectRoutes(t *testing.T) func(<-chan types.Delta, error) ([]types.RouteDelta, string) {
	return func(ch <-chan types.Delta, err error) ([]types.RouteDelta, string) { return readRoutes(t, ch, err) }
}

func readRoutes(t *testing.T, ch <-chan types.Delta, err error) ([]types.RouteDelta, string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var routes []types.RouteDelta
	var text string
	for d := range ch {
		switch v := d.(type) {
		case types.RouteDelta:
			routes = append(routes, v)
		case types.TextContentDelta:
			text += v.Content
		case types.ErrorDelta:
			t.Fatal(v.Error)
		}
	}
	return routes, text
}

func TestGroupsRestrictAndPin(t *testing.T) {
	r, err := New(Config{
		Profiles: []Profile{
			{ID: "a/1", Provider: provider{model: "m1", call: func() (<-chan types.Delta, error) { return nil, transient() }}, Preset: "a", ConfigHash: "h1", CatalogRevision: "rev"},
			{ID: "a/2", Provider: provider{model: "m2", call: ok("a2")}, Preset: "a", ConfigHash: "h2", CatalogRevision: "rev"},
			{ID: "b/1", Provider: provider{model: "m3", call: ok("b1")}, Preset: "b"},
		},
		Groups:       map[string][]string{"a": {"a/1", "a/2"}, "b": {"b/1"}},
		DefaultGroup: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session()
	routes, text := collectRoutes(t)(s.ChatStream(context.Background(), nil, nil))
	if text != "a2" || len(routes) != 2 || routes[1].Preset != "a" || routes[1].ConfigHash != "h2" || routes[1].CatalogRevision != "rev" {
		t.Fatalf("default group: %q %+v", text, routes)
	}
	pinned := s.WithModel("b")
	routes, text = collectRoutes(t)(pinned.ChatStream(context.Background(), nil, nil))
	if text != "b1" || routes[0].Reason != ReasonPinned || routes[0].Profile != "b/1" {
		t.Fatalf("group pin: %q %+v", text, routes)
	}
	if _, err := s.WithModel("nope").ChatStream(context.Background(), nil, nil); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("unknown name: %v", err)
	}
}

func TestGroupValidation(t *testing.T) {
	p := []Profile{{ID: "x", Provider: provider{call: ok("")}}, {ID: "y", Provider: provider{call: ok("")}}}
	if _, err := New(Config{Profiles: p, Groups: map[string][]string{"x": {"x"}}}); err != nil {
		t.Fatalf("a group may share its sole member's ID: %v", err)
	}
	for _, cfg := range []Config{
		{Profiles: p, Groups: map[string][]string{"x": {"x", "y"}}},
		{Profiles: p, Groups: map[string][]string{"g": {"missing"}}},
		{Profiles: p, Groups: map[string][]string{"g": {}}},
		{Profiles: p, DefaultGroup: "g"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}

// A gpt-5.2 profile configured with temperature cannot take a reasoning
// effort override: temperature needs reasoning off. Validating only the
// override would admit it and fail inside the adapter.
func TestEligibilityUsesMergedOptions(t *testing.T) {
	caps, _ := catalog.Lookup("openai", "gpt-5.2")
	temp := 0.5
	var sent []types.RequestOptions
	r, err := New(Config{Profiles: []Profile{
		{ID: "sampled", Provider: optsProvider{provider: provider{model: "gpt-5.2", caps: caps, call: ok("sampled")}, configured: types.RequestOptions{Temperature: &temp}}},
		{ID: "plain", Provider: optsProvider{provider: provider{model: "gpt-5.2", caps: caps, call: ok("plain")}, got: &sent}},
	}, Groups: map[string][]string{"chain": {"sampled", "plain"}}, DefaultGroup: "chain"})
	if err != nil {
		t.Fatal(err)
	}
	effort := "high"
	routes, text := collectRoutes(t)(r.Session().ChatStreamWithOptions(context.Background(), nil, nil, types.RequestOptions{ReasoningEffort: &effort}))
	if text != "plain" || routes[0].Reason != ReasonOptions {
		t.Fatalf("got %q %+v", text, routes)
	}
	if routes[0].Options == nil || *routes[0].Options.ReasoningEffort != "high" || routes[0].Options.Temperature != nil {
		t.Fatalf("route options: %+v", routes[0].Options)
	}
	if len(sent) != 1 || *sent[0].ReasoningEffort != "high" {
		t.Fatalf("sent %+v", sent)
	}
}
