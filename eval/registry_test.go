package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuiltinRegistryBuild(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		obs      Observation
		wantName string
		want     float64
	}{
		{"no params", `{"kind": "token_f1"}`, Observation{Output: json.RawMessage(`"a"`), GroundTruth: json.RawMessage(`"a"`)}, "token_f1", 1},
		{"renamed", `{"kind": "exact_match", "name": "em"}`, Observation{Output: json.RawMessage(`"a"`), GroundTruth: json.RawMessage(`"b"`)}, "em", 0},
		{"contains", `{"kind": "contains", "params": {"substrings": ["x"]}}`, out(`"xyz"`), "contains:x", 1},
		{"regex defaults to at least one", `{"kind": "regex_count", "params": {"pattern": "\\d"}}`, out(`"a1"`), `regex_count:\d>=1`, 1},
		{"regex op", `{"kind": "regex_count", "params": {"pattern": "a", "op": "==", "count": 2}}`, out(`"aaa"`), "regex_count:a==2", 0},
		{"json schema", `{"kind": "json_schema", "params": {"schema": {"type": "array"}}}`, out(`[]`), "json_schema", 1},
		{"json field equals", `{"kind": "json_field", "params": {"path": "a", "equals": null}}`, out(`{"a": null}`), "json_field:a", 1},
		{"json field from truth", `{"kind": "json_field", "params": {"path": "a", "from_ground_truth": true}}`,
			Observation{Output: json.RawMessage(`{"a": 2}`), GroundTruth: json.RawMessage(`{"a": 2}`)}, "json_field:a", 1},
		{"token budget", `{"kind": "token_budget", "params": {"max_tokens": 5}}`, Observation{Timing: ObservationTiming{OutputTokens: 6}}, "token_budget", 0},
		{"cost budget", `{"kind": "cost_budget", "params": {"max_usd": 0}}`, Observation{Timing: ObservationTiming{CostUSD: new(float64)}}, "cost_budget", 1},
		{"responds within", `{"kind": "responds_within", "params": {"max_ms": 10}}`, Observation{Timing: ObservationTiming{TotalMs: 5}}, "responds_within", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec ScorerSpec
			if err := json.Unmarshal([]byte(tt.spec), &spec); err != nil {
				t.Fatal(err)
			}
			s, err := BuildScorer(spec)
			if err != nil {
				t.Fatal(err)
			}
			got := scoreOne(t, s, tt.obs)
			if got.Name != tt.wantName || got.Value != tt.want {
				t.Fatalf("score = %+v, want %s %v", got, tt.wantName, tt.want)
			}
		})
	}
}

func TestRegistryBuildErrors(t *testing.T) {
	tests := []struct {
		name string
		spec ScorerSpec
		want string
	}{
		{"unknown kind", ScorerSpec{Kind: "nope"}, "unknown scorer kind"},
		{"unknown param", ScorerSpec{Kind: "token_budget", Params: json.RawMessage(`{"max": 1}`)}, `unknown field "max"`},
		{"missing param", ScorerSpec{Kind: "token_budget"}, "max_tokens is required"},
		{"params on a no-param kind", ScorerSpec{Kind: "token_f1", Params: json.RawMessage(`{"x": 1}`)}, "takes no params"},
		{"bad schema", ScorerSpec{Kind: "json_schema", Params: json.RawMessage(`{"schema": {"$ref": "x"}}`)}, "not supported"},
		{"exclusive json_field params", ScorerSpec{Kind: "json_field", Params: json.RawMessage(`{"path": "a", "equals": 1, "from_ground_truth": true}`)}, "exclusive"},
		{"bad regex op", ScorerSpec{Kind: "regex_count", Params: json.RawMessage(`{"pattern": "a", "op": ">"}`)}, "unknown op"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildScorer(tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	if _, err := BuildScorer(ScorerSpec{Kind: "nope"}); !errors.Is(err, ErrUnknownScorer) {
		t.Fatalf("unknown kind error = %v, want ErrUnknownScorer", err)
	}
}

func TestRegistryRegisterAndList(t *testing.T) {
	r := NewRegistry()
	factory := NoParams(func() Scorer {
		return NewScorerFunc("one", func(context.Context, Observation) (Score, error) { return Score{Name: "one", Value: 1}, nil })
	})
	if err := r.Register("one", "always one", factory); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("one", "again", factory); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if err := r.Register("", "x", factory); err == nil {
		t.Fatal("empty kind accepted")
	}
	scorers, err := r.BuildAll([]ScorerSpec{{Kind: "one"}, {Kind: "one", Name: "uno"}})
	if err != nil {
		t.Fatal(err)
	}
	if scorers[0].Name() != "one" || scorers[1].Name() != "uno" {
		t.Fatalf("names = %q, %q", scorers[0].Name(), scorers[1].Name())
	}
	if _, err := r.BuildAll([]ScorerSpec{{Kind: "one"}, {Kind: "two"}}); err == nil || !strings.Contains(err.Error(), "scorer 1") {
		t.Fatalf("BuildAll error = %v, want it to name scorer 1", err)
	}
	if kinds := r.Kinds(); len(kinds) != 1 || kinds[0].Description != "always one" {
		t.Fatalf("Kinds = %+v", kinds)
	}

	builtin := NewBuiltinRegistry().Kinds()
	for i := 1; i < len(builtin); i++ {
		if builtin[i-1].Kind >= builtin[i].Kind {
			t.Fatalf("Kinds not sorted: %q before %q", builtin[i-1].Kind, builtin[i].Kind)
		}
	}
	for _, k := range builtin {
		if k.Description == "" {
			t.Errorf("builtin %q has no description", k.Kind)
		}
	}
}
