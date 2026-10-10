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

func (p optsProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	if p.got != nil && req.Options != nil {
		*p.got = append(*p.got, *req.Options)
	}
	return p.call()
}

func (p optsProvider) SupportsOptions() bool                  { return true }
func (p optsProvider) EffectiveOptions() types.RequestOptions { return p.configured }

func ok(text string) func() (<-chan types.Delta, error) {
	return func() (<-chan types.Delta, error) { return deltas(types.PartDelta{Index: 0, Text: text}), nil }
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
		case types.PartDelta:
			text += v.Text
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
		Groups:       map[types.PresetName][]types.ProfileID{"a": {"a/1", "a/2"}, "b": {"b/1"}},
		DefaultGroup: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session()
	routes, text := collectRoutes(t)(s.Stream(context.Background(), types.Request{}))
	if text != "a2" || len(routes) != 2 || routes[1].Preset != "a" || routes[1].ConfigHash != "h2" || routes[1].CatalogRevision != "rev" {
		t.Fatalf("default group: %q %+v", text, routes)
	}
	pinned, err := s.WithTarget(types.PresetTarget("b"))
	if err != nil {
		t.Fatal(err)
	}
	routes, text = collectRoutes(t)(pinned.Stream(context.Background(), types.Request{}))
	if text != "b1" || routes[0].Reason != ReasonPinned || routes[0].Profile != "b/1" {
		t.Fatalf("group pin: %q %+v", text, routes)
	}
	for _, target := range []types.Target{types.ProfileTarget("nope"), types.PresetTarget("nope"), types.ModelTarget("nope")} {
		if _, err := s.WithTarget(target); !errors.Is(err, ErrUnknownProfile) || !errors.Is(err, types.ErrUnknownTarget) {
			t.Fatalf("unknown target %s: %v", target, err)
		}
	}
	if _, err := s.WithTarget(types.Target{Profile: "a/1", Preset: "a"}); !errors.Is(err, types.ErrInvalidTarget) {
		t.Fatalf("a target with two fields: %v", err)
	}
	// A model target pins the first profile, in configuration order, whose
	// provider serves that model.
	byModel, err := r.Session().WithTarget(types.ModelTarget("m3"))
	if err != nil {
		t.Fatal(err)
	}
	routes, text = collectRoutes(t)(byModel.Stream(context.Background(), types.Request{}))
	if text != "b1" || routes[0].Profile != "b/1" || routes[0].Reason != ReasonPinned {
		t.Fatalf("model pin: %q %+v", text, routes)
	}
	// A profile target pins that profile, and failover may still leave it.
	byProfile, err := r.Session().WithTarget(types.ProfileTarget("a/1"))
	if err != nil {
		t.Fatal(err)
	}
	routes, text = collectRoutes(t)(byProfile.Stream(context.Background(), types.Request{}))
	if text == "" || routes[0].Profile != "a/1" || routes[0].Reason != ReasonPinned {
		t.Fatalf("profile pin: %q %+v", text, routes)
	}
}

func TestGroupValidation(t *testing.T) {
	p := []Profile{{ID: "x", Provider: provider{call: ok("")}}, {ID: "y", Provider: provider{call: ok("")}}}
	if _, err := New(Config{Profiles: p, Groups: map[types.PresetName][]types.ProfileID{"x": {"x"}}}); err != nil {
		t.Fatalf("a group may share its sole member's ID: %v", err)
	}
	for _, cfg := range []Config{
		{Profiles: p, Groups: map[types.PresetName][]types.ProfileID{"x": {"x", "y"}}},
		{Profiles: p, Groups: map[types.PresetName][]types.ProfileID{"g": {"missing"}}},
		{Profiles: p, Groups: map[types.PresetName][]types.ProfileID{"g": {}}},
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
	}, Groups: map[types.PresetName][]types.ProfileID{"chain": {"sampled", "plain"}}, DefaultGroup: "chain"})
	if err != nil {
		t.Fatal(err)
	}
	effort := "high"
	routes, text := collectRoutes(t)(r.Session().Stream(context.Background(), types.Request{Options: &types.RequestOptions{ReasoningEffort: &effort}}))
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
