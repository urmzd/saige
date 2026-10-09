package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func scoreOne(t *testing.T, s Scorer, obs Observation) Score {
	t.Helper()
	score, err := s.Score(context.Background(), obs)
	if err != nil {
		t.Fatalf("%s: %v", s.Name(), err)
	}
	return score
}

func out(raw string) Observation { return Observation{ID: "o", Output: json.RawMessage(raw)} }

func TestCheckScorers(t *testing.T) {
	schema, err := JSONSchemaScorer(json.RawMessage(`{"type": "object", "required": ["city"], "properties": {"city": {"type": "string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	citations, err := RegexCountScorer(`\[\d+\]`, GTE, 2)
	if err != nil {
		t.Fatal(err)
	}
	atMostOne, err := RegexCountScorer(`TODO`, LTE, 1)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		scorer     Scorer
		obs        Observation
		wantName   string
		wantValue  float64
		wantReason string
		declined   bool
	}{
		{"schema object", schema, out(`{"city": "Paris"}`), "json_schema", 1, "", false},
		{"schema json string", schema, out(`"{\"city\": \"Paris\"}"`), "json_schema", 1, "", false},
		{"schema fenced string", schema, out("\"```json\\n{\\\"city\\\": \\\"Oslo\\\"}\\n```\""), "json_schema", 1, "", false},
		{"schema violation", schema, out(`{"city": 3}`), "json_schema", 0, "/city: got integer, want string", false},
		{"schema prose", schema, out(`"the city is Paris"`), "json_schema", 0, "output is not JSON", false},
		{"schema empty", schema, Observation{}, "json_schema", 0, "output is not JSON", false},

		{"field match", JSONFieldScorer("answer.city", "Paris"), out(`{"answer": {"city": "Paris"}}`), "json_field:answer.city", 1, "", false},
		{"field number form", JSONFieldScorer("n", 3), out(`{"n": 3.0}`), "json_field:n", 1, "", false},
		{"field array index", JSONFieldScorer("items.1.id", "b"), out(`{"items": [{"id": "a"}, {"id": "b"}]}`), "json_field:items.1.id", 1, "", false},
		{"field mismatch", JSONFieldScorer("n", 3), out(`{"n": 4}`), "json_field:n", 0, `field "n" is 4, want 3`, false},
		{"field missing", JSONFieldScorer("a.b", 1), out(`{"a": {}}`), "json_field:a.b", 0, `field "a.b" is missing`, false},
		{"field through scalar", JSONFieldScorer("a.b", 1), out(`{"a": 2}`), "json_field:a.b", 0, `field "a" is integer`, false},
		{"field root", JSONFieldScorer("", []any{"x"}), out(`["x"]`), "json_field:", 1, "", false},

		{"ground truth field", JSONFieldGroundTruthScorer("city"),
			Observation{Output: json.RawMessage(`{"city": "Rome"}`), GroundTruth: json.RawMessage(`{"city": "Rome"}`)}, "json_field:city", 1, "", false},
		{"ground truth absent declines", JSONFieldGroundTruthScorer("city"), out(`{"city": "Rome"}`), "", 0, "", true},

		{"regex enough", citations, out(`"see [1] and [2]"`), `regex_count:\[\d+\]>=2`, 1, "2 matches", false},
		{"regex too few", citations, out(`"see [1]"`), `regex_count:\[\d+\]>=2`, 0, "1 matches, want >= 2", false},
		{"regex at most", atMostOne, out(`"TODO TODO"`), "regex_count:TODO<=1", 0, "2 matches, want <= 1", false},

		{"contains all", ContainsScorer("Go", "fast"), out(`"Go is fast"`), "contains:Go,fast", 1, "", false},
		{"contains missing", ContainsScorer("Go", "slow"), out(`"Go is fast"`), "contains:Go,slow", 0, `missing "slow"`, false},

		{"exact match", ExactMatchScorer(), Observation{Output: json.RawMessage(`" 42 "`), GroundTruth: json.RawMessage(`"42"`)}, "exact_match", 1, "", false},
		{"exact mismatch", ExactMatchScorer(), Observation{Output: json.RawMessage(`"41"`), GroundTruth: json.RawMessage(`"42"`)}, "exact_match", 0, `got "41", want "42"`, false},
		{"exact no truth declines", ExactMatchScorer(), out(`"41"`), "", 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreOne(t, tt.scorer, tt.obs)
			if tt.declined {
				if got.Name != "" {
					t.Fatalf("score = %+v, want declined", got)
				}
				return
			}
			if got.Name != tt.wantName || got.Value != tt.wantValue {
				t.Fatalf("score = %s %v (%q), want %s %v", got.Name, got.Value, got.Reason, tt.wantName, tt.wantValue)
			}
			if !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestCheckScorerConstructionErrors(t *testing.T) {
	if _, err := JSONSchemaScorer(json.RawMessage(`{"$ref": "x"}`)); err == nil {
		t.Error("JSONSchemaScorer accepted an unsupported schema")
	}
	if _, err := RegexCountScorer("(", GTE, 1); err == nil {
		t.Error("RegexCountScorer accepted an invalid pattern")
	}
	if _, err := RegexCountScorer("a", Op(">"), 1); err == nil {
		t.Error("RegexCountScorer accepted an unknown op")
	}
}

func TestCheckScorersAreDeterministic(t *testing.T) {
	schema, _ := JSONSchemaScorer(json.RawMessage(`true`))
	regex, _ := RegexCountScorer("a", GTE, 1)
	for _, s := range []Scorer{schema, regex, JSONFieldScorer("a", 1), ContainsScorer("a"), ExactMatchScorer(),
		TokenBudgetScorer(1), CostBudgetScorer(1), RespondsWithinScorer(time.Second), CostScorer(), TotalTokensScorer()} {
		if d, ok := s.(deterministic); !ok || !d.Deterministic() {
			t.Errorf("%s is not marked deterministic", s.Name())
		}
	}
}

func TestNewCheckScorer(t *testing.T) {
	odd := NewCheckScorer("odd_length", func(_ context.Context, obs Observation) (bool, string, error) {
		text := OutputText(obs)
		if text == "" {
			return false, "", ErrNotApplicable
		}
		return len(text)%2 == 1, "length " + string(rune('0'+len(text))), nil
	})
	tests := []struct {
		name  string
		obs   Observation
		want  float64
		empty bool
	}{
		{"pass", out(`"abc"`), 1, false},
		{"fail", out(`"ab"`), 0, false},
		{"declined", Observation{}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreOne(t, odd, tt.obs)
			if tt.empty != (got.Name == "") || got.Value != tt.want {
				t.Fatalf("score = %+v", got)
			}
		})
	}
}

func TestBudgetScorers(t *testing.T) {
	timing := func(in, outTok int, ms int64, cost *float64) Observation {
		return Observation{Timing: ObservationTiming{InputTokens: in, OutputTokens: outTok, TotalMs: ms, CostUSD: cost}}
	}
	cheap, pricey := 0.01, 0.5
	tests := []struct {
		name     string
		scorer   Scorer
		obs      Observation
		want     float64
		declined bool
	}{
		{"tokens within", TokenBudgetScorer(100), timing(60, 40, 0, nil), 1, false},
		{"tokens over", TokenBudgetScorer(100), timing(60, 41, 0, nil), 0, false},
		{"tokens unrecorded", TokenBudgetScorer(100), timing(0, 0, 0, nil), 0, true},
		{"cost within", CostBudgetScorer(0.1), timing(0, 0, 0, &cheap), 1, false},
		{"cost over", CostBudgetScorer(0.1), timing(0, 0, 0, &pricey), 0, false},
		{"cost unrecorded", CostBudgetScorer(0.1), timing(0, 0, 0, nil), 0, true},
		{"time within", RespondsWithinScorer(time.Second), timing(0, 0, 1000, nil), 1, false},
		{"time over", RespondsWithinScorer(time.Second), timing(0, 0, 1001, nil), 0, false},
		{"time unrecorded", RespondsWithinScorer(time.Second), timing(0, 0, 0, nil), 0, true},
		{"cost metric", CostScorer(), timing(0, 0, 0, &pricey), 0.5, false},
		{"cost metric unrecorded", CostScorer(), timing(0, 0, 0, nil), 0, true},
		{"total tokens", TotalTokensScorer(), timing(3, 4, 0, nil), 7, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreOne(t, tt.scorer, tt.obs)
			if tt.declined != (got.Name == "") || got.Value != tt.want {
				t.Fatalf("score = %+v, want value %v declined %v", got, tt.want, tt.declined)
			}
		})
	}
}

func TestNamedScorers(t *testing.T) {
	obs := Observation{Output: json.RawMessage(`"a b"`), GroundTruth: json.RawMessage(`"a b"`)}
	tests := []struct {
		name   string
		scorer Scorer
	}{
		{"Named on a builtin", Named(TokenF1Scorer(), "f1")},
		{"WithName on ScorerFunc", NewScorerFunc("token_f1", TokenF1Scorer().(*ScorerFunc).fn).WithName("f1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.scorer.Name() != "f1" {
				t.Fatalf("Name() = %q", tt.scorer.Name())
			}
			if got := scoreOne(t, tt.scorer, obs); got.Name != "f1" || got.Value != 1 {
				t.Fatalf("score = %+v", got)
			}
		})
	}

	declining := Named(ExactMatchScorer(), "em")
	if got := scoreOne(t, declining, out(`"x"`)); got.Name != "" {
		t.Fatalf("Named turned a declined observation into %+v", got)
	}
	if d, ok := declining.(deterministic); !ok || !d.Deterministic() {
		t.Fatal("Named lost the deterministic mark")
	}
	if d, ok := Named(NewScorerFunc("x", nil), "y").(deterministic); ok && d.Deterministic() {
		t.Fatal("Named marked a sampled scorer deterministic")
	}

	used := Observation{ID: "u", Timing: ObservationTiming{InputTokens: 50, OutputTokens: 50}}
	suite, err := Run(context.Background(), "s", []Observation{used}, []Scorer{
		Named(TokenBudgetScorer(10), "tight"), Named(TokenBudgetScorer(1000), "loose"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if suite.Aggregate["tight"] != 0 || suite.Aggregate["loose"] != 1 || len(suite.Aggregate) != 2 {
		t.Fatalf("aggregate = %v, want tight=0 and loose=1 as separate metrics", suite.Aggregate)
	}
}
