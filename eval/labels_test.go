package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLabelsString(t *testing.T) {
	tests := []struct {
		labels Labels
		want   string
	}{
		{nil, ""},
		{Labels{"b": "2", "a": "1"}, "a=1,b=2"},
	}
	for _, tt := range tests {
		if got := tt.labels.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
	if (Labels{}).Clone() != nil {
		t.Error("Clone of empty labels should be nil")
	}
}

func TestWhereMatch(t *testing.T) {
	obs := Observation{Labels: Labels{"topic": "billing", "variant": "a"}}
	tests := []struct {
		name  string
		where Where
		want  bool
	}{
		{"empty matches all", nil, true},
		{"one label", Where{"topic": "billing"}, true},
		{"all labels", Where{"topic": "billing", "variant": "a"}, true},
		{"wrong value", Where{"topic": "refunds"}, false},
		{"missing label", Where{"lang": "go"}, false},
		{"empty value requires the label", Where{"lang": ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.where.Match(obs); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWhereFilter(t *testing.T) {
	results := []ObservationResult{
		{Observation: Observation{ID: "1", Labels: Labels{"t": "x"}}},
		{Observation: Observation{ID: "2", Labels: Labels{"t": "y"}}},
		{Observation: Observation{ID: "3"}},
	}
	if got := (Where{"t": "x"}).Filter(results); len(got) != 1 || got[0].Observation.ID != "1" {
		t.Errorf("Filter = %+v", got)
	}
	if got := Where(nil).Filter(results); len(got) != 3 {
		t.Errorf("empty Filter = %d results, want 3", len(got))
	}
}

func TestWhereScorer(t *testing.T) {
	inner := NewScorerFunc("m", func(context.Context, Observation) (Score, error) {
		return Score{Name: "m", Value: 1}, nil
	})
	scoped := Where{"topic": "billing"}.Scorer(inner)
	if scoped.Name() != "m" {
		t.Errorf("Name = %q", scoped.Name())
	}
	tests := []struct {
		name     string
		labels   Labels
		wantName string
	}{
		{"matching observation is scored", Labels{"topic": "billing"}, "m"},
		{"other observation is declined", Labels{"topic": "refunds"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := scoped.Score(context.Background(), Observation{Labels: tt.labels})
			if err != nil || s.Name != tt.wantName {
				t.Errorf("Score = %+v, %v; want name %q", s, err, tt.wantName)
			}
		})
	}

	if Where(nil).Scorer(inner) != Scorer(inner) {
		t.Error("an empty Where should return the scorer unchanged")
	}
	det := Where{"topic": "billing"}.Scorer(Deterministic(inner))
	if d, ok := det.(deterministic); !ok || !d.Deterministic() {
		t.Error("Where must keep the Deterministic mark")
	}
	if Sampled(det, Sampler{N: 3}) != det {
		t.Error("a deterministic scoped scorer must not be sampled")
	}
}

func TestMatrix(t *testing.T) {
	build := func(l Labels) (Subject, error) {
		return func(context.Context, *Observation) error { return nil }, nil
	}
	variants, err := Matrix(build,
		Axis{Name: "model", Values: []string{"small", "large"}},
		Axis{Name: "tools", Values: []string{"none", "search"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"small/none", "small/search", "large/none", "large/search"}
	if len(variants) != len(wantNames) {
		t.Fatalf("variants = %d, want %d", len(variants), len(wantNames))
	}
	for i, v := range variants {
		if v.Name != wantNames[i] {
			t.Errorf("variant %d name = %q, want %q", i, v.Name, wantNames[i])
		}
		parts := strings.Split(v.Name, "/")
		if v.Labels["model"] != parts[0] || v.Labels["tools"] != parts[1] {
			t.Errorf("variant %q labels = %v", v.Name, v.Labels)
		}
		if v.Subject == nil {
			t.Errorf("variant %q has no subject", v.Name)
		}
	}
}

func TestMatrixErrors(t *testing.T) {
	ok := func(Labels) (Subject, error) {
		return func(context.Context, *Observation) error { return nil }, nil
	}
	failing := func(l Labels) (Subject, error) {
		if l["model"] == "bad" {
			return nil, errors.New("unknown model")
		}
		return ok(l)
	}
	tests := []struct {
		name  string
		build func(Labels) (Subject, error)
		axes  []Axis
		want  string
	}{
		{"nil build", nil, []Axis{{Name: "m", Values: []string{"x"}}}, "nil build"},
		{"no axes", ok, nil, "no axes"},
		{"unnamed axis", ok, []Axis{{Values: []string{"x"}}}, "empty name"},
		{"duplicate axis", ok, []Axis{{Name: "m", Values: []string{"x"}}, {Name: "m", Values: []string{"y"}}}, "duplicate axis"},
		{"empty axis", ok, []Axis{{Name: "m"}}, "no values"},
		{"repeated value", ok, []Axis{{Name: "m", Values: []string{"x", "x"}}}, "repeats value"},
		{"reserved axis name", ok, []Axis{{Name: LabelVariant, Values: []string{"a"}}, {Name: "m", Values: []string{"x"}}}, "reserved"},
		{"build error", failing, []Axis{{Name: "model", Values: []string{"good", "bad"}}}, "unknown model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Matrix(tt.build, tt.axes...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
