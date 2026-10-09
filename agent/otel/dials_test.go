package otel

import (
	"context"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// A router reports each attempt's dials on its route; the span carries the
// serving attempt's decisions and policy.
func TestDialAttributesFromRoute(t *testing.T) {
	low := "low"
	rep := types.DialReport{Policy: "default", Decisions: []types.DialDecision{
		{Dial: types.DialCreativity, Requested: "focused", Action: types.DialDropped, Reason: "conflicts with effort medium"},
		{Dial: types.DialReasoning, Requested: "off", Sent: "effort=low", Action: types.DialMapped},
	}}
	inner := &fakeProvider{deltas: []types.Delta{
		types.RouteDelta{Profile: "p/a", Provider: "openai", Options: &types.RequestOptions{ReasoningEffort: &low}, Dials: &rep},
		types.TextContentDelta{Content: "ok"},
	}}
	tracer, rec := newSpyTracer()
	ch, err := NewTracedProvider(inner, tracer).ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	s := rec.named("chat m1")[0]
	for key, want := range map[string][]string{
		"saige.dials.requested": {"creativity:focused", "reasoning:off"},
		"saige.dials.mapped":    {"reasoning:off→effort=low"},
		"saige.dials.dropped":   {"creativity:focused"},
	} {
		v, ok := s.attr(key)
		if !ok || !slices.Equal(v.AsStringSlice(), want) {
			t.Errorf("%s = %v, want %v", key, v.AsStringSlice(), want)
		}
	}
	if s.str("saige.dials.policy") != "default" {
		t.Errorf("policy = %q", s.str("saige.dials.policy"))
	}
	if len(s.events) != 1 || s.events[0].attrs["saige.dials.policy"].AsString() != "default" {
		t.Fatalf("the attempt event lacks the dials: %+v", s.events)
	}
}

// capsProvider is an options provider that declares a catalog model, as a
// single adapter does.
type capsProvider struct {
	optionsProvider
	caps types.ModelCapabilities
}

func (p *capsProvider) Capabilities() types.ModelCapabilities { return p.caps }

// A single adapter reports no route, so the traced call compiles the dials
// itself and records the effective options it sends.
func TestDialAttributesForSingleAdapter(t *testing.T) {
	inner := &capsProvider{optionsProvider: optionsProvider{fakeProvider{deltas: []types.Delta{types.TextContentDelta{Content: "ok"}}}},
		caps: catalog.MustLookup("ollama", "qwen3")}
	tracer, rec := newSpyTracer()
	ch, err := NewTracedProvider(inner, tracer).ChatStreamWithOptions(context.Background(), nil, nil,
		types.RequestOptions{Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}, DialPolicy: &types.DialPolicy{}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	s := rec.named("chat m1")[0]
	if v, _ := s.attr("saige.dials.mapped"); !slices.Equal(v.AsStringSlice(), []string{"reasoning:on:high→think=true"}) {
		t.Fatalf("mapped = %v", v.AsStringSlice())
	}
}
