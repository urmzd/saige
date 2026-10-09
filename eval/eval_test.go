package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestRunCancelledReturnsPartialSuite(t *testing.T) {
	obs := make([]Observation, 100)
	for i := range obs {
		obs[i] = Observation{ID: fmt.Sprintf("o%03d", i)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var scored atomic.Int32
	scorer := NewScorerFunc("slow", func(ctx context.Context, _ Observation) (Score, error) {
		if scored.Add(1) == 10 {
			cancel()
		}
		select {
		case <-ctx.Done():
			return Score{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
		return Score{Name: "slow", Value: 1}, nil
	})

	suite, err := Run(ctx, "cancel", obs, []Scorer{scorer}, WithConcurrency(4))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if suite == nil {
		t.Fatal("expected a partial suite")
	}
	if suite.Incomplete == 0 {
		t.Error("expected Incomplete > 0")
	}
	if suite.Incomplete+len(suite.Results) != len(obs) {
		t.Errorf("Incomplete %d + Results %d != %d", suite.Incomplete, len(suite.Results), len(obs))
	}
	if suite.ErroredCases != 0 {
		t.Errorf("cancelled scorers must not count as errored cases, got %d", suite.ErroredCases)
	}
	for _, r := range suite.Results {
		if r.Observation.ID == "" {
			t.Fatal("result with empty Observation.ID")
		}
		if len(r.Scores) != 1 || r.Scores[0].Error != "" {
			t.Errorf("case %s: unexpected scores %+v", r.Observation.ID, r.Scores)
		}
	}
}

func TestRunCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	suite, err := Run(ctx, "pre", []Observation{{ID: "a"}, {ID: "b"}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if suite.Incomplete != 2 || len(suite.Results) != 0 {
		t.Errorf("got Incomplete=%d Results=%d, want 2 and 0", suite.Incomplete, len(suite.Results))
	}
}

func TestPopulateAllRecordsSubjectErrors(t *testing.T) {
	obs := make([]Observation, 10)
	for i := range obs {
		obs[i] = Observation{ID: fmt.Sprintf("o%d", i)}
	}
	var running, peak atomic.Int32
	subject := Subject(func(_ context.Context, o *Observation) error {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if o.ID == "o3" {
			return errors.New("boom")
		}
		o.Output = json.RawMessage(`"ok"`)
		return nil
	})

	err := PopulateAll(context.Background(), obs, subject, WithConcurrency(4))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected joined subject error, got %v", err)
	}
	if peak.Load() < 2 {
		t.Errorf("expected subjects to run in parallel, peak concurrency %d", peak.Load())
	}
	if peak.Load() > 4 {
		t.Errorf("concurrency limit exceeded: %d", peak.Load())
	}
	if got := SubjectError(obs[3]); got != "boom" {
		t.Errorf("SubjectError: got %q, want boom", got)
	}
	if got := SubjectError(obs[0]); got != "" {
		t.Errorf("successful observation has subject error %q", got)
	}

	scorer := NewScorerFunc("one", func(_ context.Context, _ Observation) (Score, error) {
		return Score{Name: "one", Value: 1}, nil
	})
	suite, err := Run(context.Background(), "s", obs, []Scorer{scorer})
	if err != nil {
		t.Fatal(err)
	}
	if suite.SubjectErrors != 1 || len(suite.Results) != 10 {
		t.Errorf("SubjectErrors=%d Results=%d, want 1 and 10", suite.SubjectErrors, len(suite.Results))
	}
	if len(suite.Results[3].Scores) != 0 {
		t.Errorf("failed observation must not be scored, got %+v", suite.Results[3].Scores)
	}
}
