package eval

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
)

// sequence returns a scorer that yields the given values in order, one per
// call, and counts its calls. An error value at a position fails that call.
func sequence(name string, values []float64, failAt map[int]bool) (Scorer, *atomic.Int64) {
	var calls atomic.Int64
	return NewScorerFunc(name, func(context.Context, Observation) (Score, error) {
		i := int(calls.Add(1)) - 1
		if failAt[i] {
			return Score{}, errors.New("judge unavailable")
		}
		return Score{Name: name, Value: values[i%len(values)], Reason: "because"}, nil
	}), &calls
}

func TestSampledScorer(t *testing.T) {
	tests := []struct {
		name       string
		sampler    Sampler
		values     []float64
		failAt     map[int]bool
		wantValue  float64
		wantStable bool
		wantCalls  int64
		wantErrors int
	}{
		{name: "identical samples are stable", sampler: Sampler{N: 3}, values: []float64{1}, wantValue: 1, wantStable: true, wantCalls: 3},
		{name: "any change is unstable at zero tolerance", sampler: Sampler{N: 3}, values: []float64{1, 1, 0}, wantValue: 2.0 / 3, wantStable: false, wantCalls: 3},
		{name: "tolerance absorbs small movement", sampler: Sampler{N: 3, Tolerance: 0.1}, values: []float64{0.8, 0.85, 0.9}, wantValue: 0.85, wantStable: true, wantCalls: 3},
		{name: "median resists an outlier", sampler: Sampler{N: 3, Reduce: Median}, values: []float64{1, 0, 1}, wantValue: 1, wantStable: false, wantCalls: 3},
		{name: "min is the pessimistic gate", sampler: Sampler{N: 3, Reduce: Min}, values: []float64{1, 0, 1}, wantValue: 0, wantStable: false, wantCalls: 3},
		{name: "a failed sample is counted and skipped", sampler: Sampler{N: 3}, values: []float64{1}, failAt: map[int]bool{1: true}, wantValue: 1, wantStable: true, wantCalls: 3, wantErrors: 1},
		{name: "concurrent samples all run", sampler: Sampler{N: 8, Concurrency: 4}, values: []float64{1}, wantValue: 1, wantStable: true, wantCalls: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner, calls := sequence("judge", tt.values, tt.failAt)
			got, err := Sampled(inner, tt.sampler).Score(context.Background(), Observation{ID: "a"})
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(got.Value-tt.wantValue) > 1e-9 {
				t.Errorf("value = %v, want %v", got.Value, tt.wantValue)
			}
			if got.Samples == nil || got.Samples.Stable != tt.wantStable {
				t.Errorf("samples = %+v, want stable=%v", got.Samples, tt.wantStable)
			}
			if got.Samples.Errors != tt.wantErrors {
				t.Errorf("errors = %d, want %d", got.Samples.Errors, tt.wantErrors)
			}
			if calls.Load() != tt.wantCalls {
				t.Errorf("scorer ran %d times, want %d", calls.Load(), tt.wantCalls)
			}
			if got.Name != "judge" {
				t.Errorf("name = %q", got.Name)
			}
		})
	}
}

func TestSampledScorerErrorsOnlyWhenEverySampleFails(t *testing.T) {
	inner, _ := sequence("judge", []float64{1}, map[int]bool{0: true, 1: true, 2: true})
	if _, err := Sampled(inner, Sampler{N: 3}).Score(context.Background(), Observation{}); err == nil {
		t.Fatal("want an error when all samples fail")
	}
}

func TestSampledIsANoOpWhereItCannotHelp(t *testing.T) {
	inner, calls := sequence("exact", []float64{1}, nil)
	for name, scorer := range map[string]Scorer{
		"n below two":          Sampled(inner, Sampler{N: 1}),
		"deterministic scorer": Sampled(Deterministic(inner), Sampler{N: 5}),
		"zero value sampler":   Sampled(inner, Sampler{}),
	} {
		calls.Store(0)
		got, err := scorer.Score(context.Background(), Observation{})
		if err != nil || got.Samples != nil || calls.Load() != 1 {
			t.Errorf("%s: calls=%d samples=%v err=%v, want one plain call", name, calls.Load(), got.Samples, err)
		}
	}
}

func TestRunWithSamplerCountsUnstableScores(t *testing.T) {
	flaky, _ := sequence("judge", []float64{1, 0}, nil)
	steady, steadyCalls := sequence("exact", []float64{1}, nil)

	suite, err := Run(context.Background(), "suite", []Observation{{ID: "a"}, {ID: "b"}},
		[]Scorer{flaky, Deterministic(steady)}, WithSampler(Sampler{N: 4}))
	if err != nil {
		t.Fatal(err)
	}
	if suite.UnstableScores != 2 {
		t.Errorf("unstable scores = %d, want 2 (the judge, once per observation)", suite.UnstableScores)
	}
	if steadyCalls.Load() != 2 {
		t.Errorf("deterministic scorer ran %d times, want once per observation", steadyCalls.Load())
	}
	if got := suite.Aggregate["judge"]; math.Abs(got-0.5) > 1e-9 {
		t.Errorf("aggregate judge = %v, want 0.5", got)
	}
}

func TestReplicate(t *testing.T) {
	in := []Observation{{ID: "a"}, {ID: "b"}}
	got := Sampler{N: 3}.Replicate(in)
	if len(got) != 6 || got[0].ID != "a" || got[0].Sample != 1 || got[2].Sample != 3 || got[3].ID != "b" {
		t.Fatalf("replicate = %+v", got)
	}
	if same := (Sampler{N: 1}).Replicate(in); len(same) != 2 || same[0].Sample != 0 {
		t.Errorf("n below two must leave observations unchanged: %+v", same)
	}
}
