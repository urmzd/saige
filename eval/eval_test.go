package eval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRunSingleObservation(t *testing.T) {
	obs := []Observation{
		{
			ID:     "test-1",
			Output: json.RawMessage(`"hello world"`),
		},
	}

	constant := NewScorerFunc("always_one", func(_ context.Context, _ Observation) (Score, error) {
		return Score{Name: "always_one", Value: 1.0}, nil
	})

	result, err := Run(context.Background(), "test-suite", obs, []Scorer{constant})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if len(result.Results[0].Scores) != 1 {
		t.Fatalf("expected 1 score, got %d", len(result.Results[0].Scores))
	}
	if result.Results[0].Scores[0].Value != 1.0 {
		t.Errorf("expected score 1.0, got %f", result.Results[0].Scores[0].Value)
	}
	if result.Aggregate["always_one"] != 1.0 {
		t.Errorf("expected aggregate 1.0, got %f", result.Aggregate["always_one"])
	}
}

func TestRunMultipleObservations(t *testing.T) {
	obs := []Observation{
		{ID: "a", Output: json.RawMessage(`"foo"`)},
		{ID: "b", Output: json.RawMessage(`"bar"`)},
	}

	counter := 0.0
	scorer := NewScorerFunc("incremental", func(_ context.Context, _ Observation) (Score, error) {
		counter += 0.5
		return Score{Name: "incremental", Value: counter}, nil
	})

	result, err := Run(context.Background(), "multi", obs, []Scorer{scorer}, WithConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}
	// With concurrency=1, scores should be 0.5 and 1.0, mean = 0.75
	if result.Aggregate["incremental"] != 0.75 {
		t.Errorf("expected aggregate 0.75, got %f", result.Aggregate["incremental"])
	}
}

func TestRunSkipsEmptyScores(t *testing.T) {
	obs := []Observation{{ID: "x"}}

	// Returns empty name → should be skipped.
	noop := NewScorerFunc("noop", func(_ context.Context, _ Observation) (Score, error) {
		return Score{}, nil
	})

	result, err := Run(context.Background(), "skip", obs, []Scorer{noop})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Results[0].Scores) != 0 {
		t.Errorf("expected 0 scores, got %d", len(result.Results[0].Scores))
	}
}

func TestRunScorerErrorDoesNotAbort(t *testing.T) {
	obs := []Observation{
		{ID: "good-1"},
		{ID: "bad"},
		{ID: "good-2"},
	}

	flaky := NewScorerFunc("flaky", func(_ context.Context, o Observation) (Score, error) {
		if o.ID == "bad" {
			return Score{}, errors.New("judge unavailable")
		}
		return Score{Name: "flaky", Value: 1.0}, nil
	})
	steady := NewScorerFunc("steady", func(_ context.Context, _ Observation) (Score, error) {
		return Score{Name: "steady", Value: 0.5}, nil
	})

	result, err := Run(context.Background(), "partial", obs, []Scorer{flaky, steady}, WithConcurrency(1))
	if err != nil {
		t.Fatalf("suite should complete despite scorer error, got %v", err)
	}

	if len(result.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result.Results))
	}

	// Errored case records the failure and keeps the other scorer's score.
	bad := result.Results[1]
	var errored *Score
	for i := range bad.Scores {
		if bad.Scores[i].Name == "flaky" {
			errored = &bad.Scores[i]
		}
	}
	if errored == nil {
		t.Fatal("expected errored flaky score on bad case")
	}
	if errored.Error != "judge unavailable" {
		t.Errorf("expected recorded error, got %q", errored.Error)
	}
	steadyScored := false
	for _, s := range bad.Scores {
		if s.Name == "steady" && s.Error == "" {
			steadyScored = true
		}
	}
	if !steadyScored {
		t.Error("expected steady scorer to still score the errored case")
	}

	// Aggregate excludes the errored score instead of counting it as zero.
	if result.Aggregate["flaky"] != 1.0 {
		t.Errorf("expected flaky aggregate 1.0 (errored case excluded), got %f", result.Aggregate["flaky"])
	}
	if result.Aggregate["steady"] != 0.5 {
		t.Errorf("expected steady aggregate 0.5, got %f", result.Aggregate["steady"])
	}
	if result.ErroredCases != 1 {
		t.Errorf("expected 1 errored case, got %d", result.ErroredCases)
	}
}

func TestRunAllScoresErrored(t *testing.T) {
	obs := []Observation{{ID: "a"}, {ID: "b"}}

	broken := NewScorerFunc("broken", func(_ context.Context, _ Observation) (Score, error) {
		return Score{}, errors.New("boom")
	})

	result, err := Run(context.Background(), "all-error", obs, []Scorer{broken})
	if err == nil {
		t.Fatal("expected suite-level error when every score errors")
	}
	if result == nil {
		t.Fatal("expected suite result with per-case errors alongside the error")
	}
	if result.ErroredCases != 2 {
		t.Errorf("expected 2 errored cases, got %d", result.ErroredCases)
	}
	if _, ok := result.Aggregate["broken"]; ok {
		t.Error("errored scores must not appear in the aggregate")
	}
	for _, r := range result.Results {
		if len(r.Scores) != 1 || r.Scores[0].Error == "" {
			t.Errorf("observation %q: expected one errored score, got %+v", r.Observation.ID, r.Scores)
		}
	}
}

func TestPopulate(t *testing.T) {
	obs := []Observation{
		{ID: "p1", Input: json.RawMessage(`"input1"`)},
		{ID: "p2", Input: json.RawMessage(`"input2"`)},
	}

	subject := Subject(func(_ context.Context, o *Observation) error {
		o.Output = json.RawMessage(`"processed"`)
		o.Timing.TotalMs = 42
		return nil
	})

	if err := Populate(context.Background(), obs, subject); err != nil {
		t.Fatal(err)
	}

	for _, o := range obs {
		if string(o.Output) != `"processed"` {
			t.Errorf("expected processed output, got %s", o.Output)
		}
		if o.Timing.TotalMs != 42 {
			t.Errorf("expected 42ms, got %d", o.Timing.TotalMs)
		}
	}
}
