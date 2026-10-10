package otel

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
)

func TestRouteAttemptsAreSpanEvents(t *testing.T) {
	t1, t2 := 0.9, 0.2
	inner := &fakeProvider{deltas: []types.Delta{
		types.RouteDelta{Profile: "p/a", Provider: "openai", Preset: "p", ConfigHash: "ha", CatalogRevision: "rev",
			Options: &types.RequestOptions{Temperature: &t1}},
		types.RouteDelta{Profile: "p/b", Provider: "google", Preset: "p", ConfigHash: "hb", CatalogRevision: "rev", Reason: "failover",
			Options: &types.RequestOptions{Temperature: &t2}},
		types.PartDelta{Index: 0, Text: "ok"},
	}}
	tracer, rec := newSpyTracer()
	ch, err := NewTracedProvider(inner, tracer).Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	s := rec.named("chat m1")[0]
	if len(s.events) != 2 || s.events[0].name != "saige.route.attempt" {
		t.Fatalf("events %+v", s.events)
	}
	if v := s.events[0].attrs["gen_ai.request.temperature"]; v.AsFloat64() != 0.9 {
		t.Fatalf("first attempt temperature %v", v)
	}
	if v, _ := s.attr("gen_ai.request.temperature"); v.AsFloat64() != 0.2 {
		t.Fatalf("span must carry the serving attempt's options, got %v", v)
	}
	for key, want := range map[string]string{"saige.route.preset": "p", "saige.route.config_hash": "hb", "saige.catalog.revision": "rev"} {
		if got := s.str(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestProviderNameSkipsDecorators(t *testing.T) {
	inner := retry.New(&fakeProvider{deltas: []types.Delta{types.PartDelta{Index: 0, Text: "ok"}}}, retry.DefaultConfig())
	tracer, rec := newSpyTracer()
	ch, err := NewTracedProvider(inner, tracer).Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if got := rec.named("chat m1")[0].str("gen_ai.provider.name"); got != "fake" {
		t.Fatalf("gen_ai.provider.name = %q, want the adapter beneath the retry decorator", got)
	}
}

func TestConversionReportsAreSpanEventsAndAttributes(t *testing.T) {
	rep := types.ConversionReport{Offering: "anthropic/claude-haiku-5-5@anthropic", Hash: "h1", Decisions: []types.ConversionDecision{
		{Path: types.PartPath{Message: 1, Part: 1, Nested: -1}, Kind: types.KindAudio, Action: types.DecisionTranscribed, Via: "transcribe@1"}}}
	inner := &fakeProvider{deltas: []types.Delta{
		types.RouteDelta{Profile: "p/a"},
		types.ConversionDelta{Profile: "p/a", Report: rep},
		types.PartDelta{Index: 0, Text: "ok"},
	}}
	tracer, rec := newSpyTracer()
	ch, err := NewTracedProvider(inner, tracer).Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	s := rec.named("chat m1")[0]
	if len(s.events) != 2 || s.events[1].name != "saige.conversion" {
		t.Fatalf("events %+v", s.events)
	}
	if s.str("saige.conversion.hash") != "h1" || s.str("saige.conversion.offering") != rep.Offering {
		t.Fatalf("conversion attributes missing")
	}
	if v, _ := s.attr("saige.conversion.decisions"); len(v.AsStringSlice()) != 1 || v.AsStringSlice()[0] != "1.1:transcribed:transcribe@1" {
		t.Fatalf("decisions = %v", v.AsStringSlice())
	}
}
