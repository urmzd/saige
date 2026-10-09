package eval

import (
	"math"
	"testing"
)

// groupResults builds results for two topics and two variants.
func groupResults() []ObservationResult {
	row := func(id, topic, variant string, v float64, passed *bool) ObservationResult {
		return ObservationResult{
			Observation: Observation{ID: id, Labels: Labels{"topic": topic, LabelVariant: variant}},
			Scores:      []Score{{Name: "m", Value: v, Passed: passed}},
		}
	}
	return []ObservationResult{
		row("1", "billing", "a", 1, ptr(true)),
		row("2", "billing", "a", 0, ptr(false)),
		row("3", "refunds", "a", 1, ptr(true)),
		row("1", "billing", "b", 1, ptr(true)),
		row("2", "billing", "b", 1, ptr(true)),
		row("3", "refunds", "b", 0.5, nil),
		{Observation: Observation{ID: "4", Labels: Labels{"topic": "refunds", LabelVariant: "b"}},
			Scores: []Score{{Name: "m", Error: "boom"}}},
	}
}

func TestGroupBy(t *testing.T) {
	tests := []struct {
		name     string
		by       []string
		want     map[string]float64 // labels string -> mean
		wantN    map[string]int
		wantComp map[string]float64
	}{
		{
			name:     "no labels is one group",
			want:     map[string]float64{"": 4.5 / 6},
			wantN:    map[string]int{"": 6},
			wantComp: map[string]float64{"": 6.0 / 7},
		},
		{
			name:     "by variant",
			by:       []string{LabelVariant},
			want:     map[string]float64{"variant=a": 2.0 / 3, "variant=b": 2.5 / 3},
			wantN:    map[string]int{"variant=a": 3, "variant=b": 3},
			wantComp: map[string]float64{"variant=a": 1, "variant=b": 0.75},
		},
		{
			name: "by topic and variant",
			by:   []string{"topic", LabelVariant},
			want: map[string]float64{
				"topic=billing,variant=a": 0.5, "topic=billing,variant=b": 1,
				"topic=refunds,variant=a": 1, "topic=refunds,variant=b": 0.5,
			},
			wantN: map[string]int{
				"topic=billing,variant=a": 2, "topic=billing,variant=b": 2,
				"topic=refunds,variant=a": 1, "topic=refunds,variant=b": 1,
			},
			wantComp: map[string]float64{
				"topic=billing,variant=a": 1, "topic=billing,variant=b": 1,
				"topic=refunds,variant=a": 1, "topic=refunds,variant=b": 0.5,
			},
		},
		{
			name:     "missing label groups under empty value",
			by:       []string{"lang"},
			want:     map[string]float64{"lang=": 4.5 / 6},
			wantN:    map[string]int{"lang=": 6},
			wantComp: map[string]float64{"lang=": 6.0 / 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := GroupBy(groupResults(), "m", tt.by...)
			if len(groups) != len(tt.want) {
				t.Fatalf("groups = %+v, want %d", groups, len(tt.want))
			}
			for i, g := range groups {
				key := g.Labels.String()
				if i > 0 && groups[i-1].Labels.String() >= key {
					t.Errorf("groups not sorted: %q before %q", groups[i-1].Labels, key)
				}
				if math.Abs(g.Stat.Value-tt.want[key]) > 1e-9 || g.Stat.N != tt.wantN[key] {
					t.Errorf("%s = %+v, want mean %v over %d", key, g.Stat, tt.want[key], tt.wantN[key])
				}
				if math.Abs(g.Stat.Completeness-tt.wantComp[key]) > 1e-9 {
					t.Errorf("%s completeness = %v, want %v", key, g.Stat.Completeness, tt.wantComp[key])
				}
				if g.Stat.Low > g.Stat.Value || g.Stat.High < g.Stat.Value {
					t.Errorf("%s interval [%v, %v] excludes the mean", key, g.Stat.Low, g.Stat.High)
				}
			}
		})
	}
}

func TestGroupPassRate(t *testing.T) {
	groups := GroupPassRate(groupResults(), "m", LabelVariant)
	tests := []struct {
		variant    string
		rate       float64
		n          int
		completion float64
	}{
		{"a", 2.0 / 3, 3, 1},
		{"b", 1, 2, 0.5}, // one score ungated, one errored without a verdict
	}
	if len(groups) != len(tests) {
		t.Fatalf("groups = %+v", groups)
	}
	for i, tt := range tests {
		g := groups[i]
		if g.Labels[LabelVariant] != tt.variant {
			t.Fatalf("group %d = %v, want variant %s", i, g.Labels, tt.variant)
		}
		if math.Abs(g.Stat.Value-tt.rate) > 1e-9 || g.Stat.N != tt.n || math.Abs(g.Stat.Completeness-tt.completion) > 1e-9 {
			t.Errorf("variant %s = %+v, want rate %v n %d completeness %v", tt.variant, g.Stat, tt.rate, tt.n, tt.completion)
		}
		if g.Stat.Low < 0 || g.Stat.High > 1 || !g.Stat.Contains(g.Stat.Value) {
			t.Errorf("variant %s Wilson interval [%v, %v] is off", tt.variant, g.Stat.Low, g.Stat.High)
		}
	}
}

func TestQuestionPassRates(t *testing.T) {
	sample := func(id string, n int, passed bool) ObservationResult {
		return ObservationResult{
			Observation: Observation{ID: id, Sample: n},
			Scores:      []Score{{Name: "m", Passed: ptr(passed)}},
		}
	}
	results := []ObservationResult{
		sample("q1", 1, true), sample("q1", 2, false), sample("q1", 3, false), sample("q1", 4, true),
		sample("q2", 1, false), sample("q2", 2, false),
		{Observation: Observation{ID: "q3"}, Scores: []Score{{Name: "m", Value: 1}}}, // never gated
	}
	rates := QuestionPassRates(results, "m", 1, 2, 5)
	tests := []struct {
		id      string
		rate    float64
		passed  int
		pass1   float64
		pass2   float64
		hasPass bool // whether pass@5 is defined
	}{
		{"q1", 0.5, 2, 0.5, 1 - 1.0/6, false},
		{"q2", 0, 0, 0, 0, false},
	}
	if len(rates) != len(tests) {
		t.Fatalf("rates = %+v, want %d questions", rates, len(tests))
	}
	for i, tt := range tests {
		q := rates[i]
		if q.ID != tt.id || q.Passed != tt.passed || math.Abs(q.Rate.Value-tt.rate) > 1e-9 {
			t.Errorf("question %d = %+v, want %s rate %v", i, q, tt.id, tt.rate)
		}
		if math.Abs(q.PassAtK[1]-tt.pass1) > 1e-9 || math.Abs(q.PassAtK[2]-tt.pass2) > 1e-9 {
			t.Errorf("%s pass@k = %v, want pass@1 %v pass@2 %v", tt.id, q.PassAtK, tt.pass1, tt.pass2)
		}
		if _, ok := q.PassAtK[5]; ok != tt.hasPass {
			t.Errorf("%s pass@5 present = %v, want %v", tt.id, ok, tt.hasPass)
		}
	}
}

func TestPassAtKByVariant(t *testing.T) {
	sample := func(variant, id string, n int, passed bool) ObservationResult {
		return ObservationResult{
			Observation: Observation{ID: id, Sample: n, Labels: Labels{LabelVariant: variant}},
			Scores:      []Score{{Name: "m", Passed: ptr(passed)}},
		}
	}
	results := []ObservationResult{
		sample("a", "q1", 1, true), sample("a", "q1", 2, false),
		sample("a", "q2", 1, false), sample("a", "q2", 2, false),
		sample("b", "q1", 1, true), sample("b", "q1", 2, true),
		sample("b", "q2", 1, true), // one sample only: pass@2 undefined
	}
	got := PassAtK(results, "m", 2)
	tests := []struct {
		variant      string
		value        float64
		n            int
		completeness float64
	}{
		{"a", 0.5, 2, 1},
		{"b", 1, 1, 0.5},
	}
	for _, tt := range tests {
		s := got[tt.variant]
		if math.Abs(s.Value-tt.value) > 1e-9 || s.N != tt.n || math.Abs(s.Completeness-tt.completeness) > 1e-9 {
			t.Errorf("variant %s pass@2 = %+v, want %v over %d (completeness %v)", tt.variant, s, tt.value, tt.n, tt.completeness)
		}
	}
}
