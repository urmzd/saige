package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRouteDeltaWireCarriesProvenance(t *testing.T) {
	temp, seed := 0.3, int64(7)
	d := RouteDelta{Profile: "balanced/openai", Provider: "openai", Model: "gpt-6-luna", Preset: "balanced",
		ConfigHash: "0123456789abcdef", CatalogRevision: "2026-10-09.1",
		Options: &RequestOptions{Temperature: &temp, Seed: &seed, ToolChoice: &ToolChoice{Mode: ToolChoiceNone}}}
	b, err := MarshalDelta(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"config_hash":"0123456789abcdef"`) || !strings.Contains(string(b), `"temperature":0.3`) {
		t.Fatalf("wire: %s", b)
	}
	got, err := UnmarshalDelta(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("round trip\n got %#v\nwant %#v", got, d)
	}
	old, err := MarshalDelta(RouteDelta{Profile: "p", Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(old), "options") || strings.Contains(string(old), "preset") {
		t.Fatalf("empty provenance must be omitted: %s", old)
	}
	if got, err := UnmarshalDelta(old); err != nil || got.(RouteDelta).Options != nil {
		t.Fatalf("old envelope: %v %#v", err, got)
	}
}

func TestRouteContentJSON(t *testing.T) {
	n := int64(512)
	c := RoutePartFrom(RouteDelta{Profile: "p/a", Preset: "p", ConfigHash: "h", CatalogRevision: "r",
		Options: &RequestOptions{MaxOutputTokens: &n}})
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var got RoutePart
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("got %#v want %#v (%s)", got, c, b)
	}
	var legacy RoutePart
	if err := json.Unmarshal([]byte(`{"profile":"x","model":"m"}`), &legacy); err != nil || legacy.Profile != "x" || legacy.Options != nil {
		t.Fatalf("legacy: %v %#v", err, legacy)
	}
}

func TestRouteDialReportRoundTrips(t *testing.T) {
	low := "low"
	rep := &DialReport{Requested: Dials{Reasoning: &ReasoningDial{Mode: ReasoningOff}}, Effective: RequestOptions{ReasoningEffort: &low},
		EffectiveHash: "abc", Policy: "default",
		Decisions: []DialDecision{{Dial: DialReasoning, Requested: "off", Sent: "effort=low", Action: DialMapped, Reason: "cannot disable", Scope: DialScopeAgent}}}
	d := RouteDelta{Profile: "p", Provider: "openai", Options: &RequestOptions{ReasoningEffort: &low}, Dials: rep}
	b, err := MarshalDelta(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalDelta(b)
	if err != nil || !reflect.DeepEqual(got, d) {
		t.Fatalf("delta round trip: %v\n got %#v\nwant %#v", err, got, d)
	}
	c := RoutePartFrom(d)
	b, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back RoutePart
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, c) {
		t.Fatalf("content round trip: %v\n got %#v\nwant %#v (%s)", err, back, c, b)
	}
}
