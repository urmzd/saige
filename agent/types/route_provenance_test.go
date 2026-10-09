package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRouteDeltaWireCarriesProvenance(t *testing.T) {
	temp, seed := 0.3, int64(7)
	d := RouteDelta{Profile: "balanced/openai", Provider: "openai", Model: "gpt-4.1", Preset: "balanced",
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
	c := RouteContentFrom(RouteDelta{Profile: "p/a", Preset: "p", ConfigHash: "h", CatalogRevision: "r",
		Options: &RequestOptions{MaxOutputTokens: &n}})
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var got RouteContent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("got %#v want %#v (%s)", got, c, b)
	}
	var legacy RouteContent
	if err := json.Unmarshal([]byte(`{"profile":"x","model":"m"}`), &legacy); err != nil || legacy.Profile != "x" || legacy.Options != nil {
		t.Fatalf("legacy: %v %#v", err, legacy)
	}
}
