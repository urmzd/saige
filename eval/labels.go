package eval

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// LabelVariant is the label [Experiment.Run] sets to the [Variant] name on
// every observation, so results of different variants can be told apart,
// grouped, and compared.
const LabelVariant = "variant"

// Labels are string key-value pairs that place an observation in a dataset
// and an experiment matrix: case metadata such as a topic or difficulty, and
// variant coordinates such as a model or toolset.
type Labels map[string]string

// Clone returns a copy of l, or nil when l is empty.
func (l Labels) Clone() Labels {
	if len(l) == 0 {
		return nil
	}
	return maps.Clone(l)
}

// String renders the labels as sorted key=value pairs joined by commas.
func (l Labels) String() string {
	keys := slices.Sorted(maps.Keys(l))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + l[k]
	}
	return strings.Join(parts, ",")
}

// merge returns a copy of l with over's pairs applied on top.
func (l Labels) merge(over Labels) Labels {
	out := make(Labels, len(l)+len(over))
	maps.Copy(out, l)
	maps.Copy(out, over)
	return out
}

// Where selects observations by label: an observation matches when every
// listed label has the listed value. An empty Where matches everything. It
// scopes assertions ([Assertion.Where]), scorers ([Where.Scorer]), and
// result slices ([Where.Filter]).
type Where map[string]string

// Match reports whether obs carries every label in w.
func (w Where) Match(obs Observation) bool {
	for k, v := range w {
		got, ok := obs.Labels[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// Filter returns the results whose observation matches w.
func (w Where) Filter(results []ObservationResult) []ObservationResult {
	if len(w) == 0 {
		return results
	}
	var out []ObservationResult
	for _, r := range results {
		if w.Match(r.Observation) {
			out = append(out, r)
		}
	}
	return out
}

// String renders the selector like [Labels.String].
func (w Where) String() string { return Labels(w).String() }

// Scorer wraps s so it scores only observations that match w and declines
// the rest (returns a zero [Score]), as a scorer does for an observation it
// does not apply to. A [Deterministic] mark on s is kept.
func (w Where) Scorer(s Scorer) Scorer {
	if len(w) == 0 {
		return s
	}
	return &whereScorer{where: w.clone(), inner: s}
}

func (w Where) clone() Where { return Where(Labels(w).Clone()) }

type whereScorer struct {
	where Where
	inner Scorer
}

func (s *whereScorer) Name() string { return s.inner.Name() }

func (s *whereScorer) Score(ctx context.Context, obs Observation) (Score, error) {
	if !s.where.Match(obs) {
		return Score{}, nil
	}
	return s.inner.Score(ctx, obs)
}

// Deterministic reports whether the wrapped scorer is marked [Deterministic].
func (s *whereScorer) Deterministic() bool {
	d, ok := s.inner.(deterministic)
	return ok && d.Deterministic()
}

// Variant is one labeled system under test in an [Experiment]. Labels hold
// its coordinates, such as {"model": "small", "tools": "none"}; Name must
// be unique within the experiment and is stamped on results under
// [LabelVariant].
type Variant struct {
	Name    string
	Labels  Labels
	Subject Subject
}

// Axis is one dimension of a [Matrix], such as a "model" axis with one
// value per model under test.
type Axis struct {
	Name   string
	Values []string
}

// Matrix builds one [Variant] per combination of axis values, in axis order
// with the last axis varying fastest. Each variant's Labels map axis names
// to its values, its Name joins the values with "/", and build turns the
// labels into the subject to run. A control, such as a tool-free variant, is
// declared as an ordinary axis value. An axis may not be named
// [LabelVariant], which holds the variant name.
func Matrix(build func(Labels) (Subject, error), axes ...Axis) ([]Variant, error) {
	if build == nil {
		return nil, fmt.Errorf("matrix: nil build function")
	}
	if len(axes) == 0 {
		return nil, fmt.Errorf("matrix: no axes")
	}
	seenAxis := make(map[string]bool, len(axes))
	for _, ax := range axes {
		if ax.Name == "" {
			return nil, fmt.Errorf("matrix: axis with empty name")
		}
		if ax.Name == LabelVariant {
			return nil, fmt.Errorf("matrix: axis name %q is reserved for the variant name", LabelVariant)
		}
		if seenAxis[ax.Name] {
			return nil, fmt.Errorf("matrix: duplicate axis %q", ax.Name)
		}
		seenAxis[ax.Name] = true
		if len(ax.Values) == 0 {
			return nil, fmt.Errorf("matrix: axis %q has no values", ax.Name)
		}
		seenValue := make(map[string]bool, len(ax.Values))
		for _, v := range ax.Values {
			if seenValue[v] {
				return nil, fmt.Errorf("matrix: axis %q repeats value %q", ax.Name, v)
			}
			seenValue[v] = true
		}
	}

	combos := [][]string{nil}
	for _, ax := range axes {
		next := make([][]string, 0, len(combos)*len(ax.Values))
		for _, prefix := range combos {
			for _, v := range ax.Values {
				next = append(next, append(slices.Clone(prefix), v))
			}
		}
		combos = next
	}

	variants := make([]Variant, 0, len(combos))
	for _, values := range combos {
		labels := make(Labels, len(axes))
		for i, ax := range axes {
			labels[ax.Name] = values[i]
		}
		subject, err := build(labels.Clone())
		if err != nil {
			return nil, fmt.Errorf("matrix: build %s: %w", labels, err)
		}
		variants = append(variants, Variant{
			Name:    strings.Join(values, "/"),
			Labels:  labels,
			Subject: subject,
		})
	}
	return variants, nil
}
