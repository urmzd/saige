package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

func catalogProfile(id, vendor, model string, call func() (<-chan types.Delta, error), got *[]types.RequestOptions) Profile {
	caps := catalog.MustLookup(types.ProviderName(vendor), model)
	return Profile{ID: types.ProfileID(id), Provider: optsProvider{provider: provider{model: model, call: call, caps: caps}, got: got}}
}

func focusedDials() types.RequestOptions {
	c := types.CreativityFocused
	return types.RequestOptions{Dials: types.Dials{Creativity: &c}}
}

// Scenario (a): creativity "focused" on a chain from gpt-6-luna to
// claude-haiku-5-5. Neither model can take it, but it is advisory: no
// member is skipped, failover proceeds, and both drops are recorded.
func TestFailoverRecordsAdvisoryDrops(t *testing.T) {
	r, err := New(Config{Profiles: []Profile{
		catalogProfile("luna", "openai", "gpt-6-luna", func() (<-chan types.Delta, error) { return nil, transient() }, nil),
		catalogProfile("haiku", "anthropic", "claude-haiku-5-5", ok("served"), nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	routes, text := collectRoutes(t)(r.Session().Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Options: new(focusedDials())}))
	if text != "served" || len(routes) != 2 {
		t.Fatalf("text %q, routes %+v", text, routes)
	}
	if routes[0].Reason != "" || routes[1].Reason != ReasonFailover {
		t.Fatalf("a member was skipped: %+v", routes)
	}
	for _, rd := range routes {
		d, ok := rd.Dials.Decision(types.DialCreativity)
		if rd.Dials == nil || !ok || d.Action != types.DialDropped {
			t.Fatalf("%s: creativity not recorded as dropped: %+v", rd.Profile, rd.Dials)
		}
		if rd.Options == nil || rd.Options.Temperature != nil {
			t.Fatalf("%s: effective options carry a temperature: %+v", rd.Profile, rd.Options)
		}
	}
}

// Scenario (a) with a raw temperature: both members reject it, so no
// member is eligible and the error names each one's reason.
func TestRawTemperatureFailsEveryMember(t *testing.T) {
	r, err := New(Config{Profiles: []Profile{
		catalogProfile("luna", "openai", "gpt-6-luna", ok("x"), nil),
		catalogProfile("haiku", "anthropic", "claude-haiku-5-5", ok("x"), nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	temp := 0.2
	_, err = r.Session().Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Options: &types.RequestOptions{Temperature: &temp}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"profile luna", "requires reasoning to be disabled", "profile haiku", "not declared supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// Scenario (b): a seed is contractual, so a member without one is filtered
// out and the next member serves with the seed applied.
func TestContractualSeedFiltersMember(t *testing.T) {
	var got []types.RequestOptions
	r, err := New(Config{
		Profiles: []Profile{
			catalogProfile("haiku", "anthropic", "claude-haiku-5-5", ok("x"), &got),
			catalogProfile("gpt41", "openai", "gpt-4.1", ok("served"), &got),
		},
		Groups: map[types.PresetName][]types.ProfileID{"chain": {"haiku", "gpt41"}}, DefaultGroup: "chain",
	})
	if err != nil {
		t.Fatal(err)
	}
	seed := int64(42)
	routes, text := collectRoutes(t)(r.Session().Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Options: &types.RequestOptions{Dials: types.Dials{Seed: &seed}}}))
	if text != "served" || len(routes) != 1 || routes[0].Profile != "gpt41" || routes[0].Reason != ReasonOptions {
		t.Fatalf("text %q routes %+v", text, routes)
	}
	if d, _ := routes[0].Dials.Decision(types.DialReproducible); d.Action != types.DialApplied || *routes[0].Options.Seed != 42 {
		t.Fatalf("seed: %+v %+v", d, routes[0].Options)
	}
	if len(got) != 1 || got[0].Dials.Seed == nil {
		t.Fatalf("the dial must reach the adapter, which compiles it again: %+v", got)
	}
}

// Configured dials compile even for a call without options, and a
// reasoning change on a row that declares a cache reset is reported.
func TestConfiguredDialsAndCacheReset(t *testing.T) {
	cfg := func(d types.Depth) types.RequestOptions {
		return types.RequestOptions{DialLayers: []types.DialLayer{{Scope: types.DialScopeEntry, Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: d}}}}}
	}
	p := optsProvider{provider: provider{model: "claude-sonnet-4-5", call: ok("x"), caps: catalog.MustLookup("anthropic", "claude-sonnet-4-5")},
		configured: cfg(types.DepthLow)}
	r, err := New(Config{Profiles: []Profile{{ID: "sonnet", Provider: p}}})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session()
	routes, _ := collectRoutes(t)(s.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}}))
	if d, ok := routes[0].Dials.Decision(types.DialReasoning); !ok || d.Action != types.DialApplied || d.CacheResetExpected {
		t.Fatalf("first call: %+v", routes[0].Dials)
	}
	routes, _ = collectRoutes(t)(s.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Options: &types.RequestOptions{Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}}}))
	d, _ := routes[0].Dials.Decision(types.DialReasoning)
	if !d.CacheResetExpected || d.Scope != types.DialScopeRequest {
		t.Fatalf("second call: %+v", d)
	}
}
