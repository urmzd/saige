// Package eval provides a universal evaluation framework for SAIGE subsystems.
//
// The framework is built on three abstractions:
//   - [Observation] — a universal eval case carrying typed I/O as JSON
//   - [Scorer] — an interface for computing a named metric from an Observation
//   - [Subject] — a function that populates an Observation's output and annotations
//
// Subsystem-specific scorers live in sub-packages (ragscore, agentscore, kgscore)
// and operate on well-known annotation keys set by their respective subjects.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Observation is the universal eval case. Input, Output, and GroundTruth use
// json.RawMessage so the same structure works for RAG queries, agent
// conversations, KG episodes, or end-to-end flows.
type Observation struct {
	ID          string                     `json:"id"`
	Turn        int                        `json:"turn"`
	Input       json.RawMessage            `json:"input"`
	Output      json.RawMessage            `json:"output"`
	GroundTruth json.RawMessage            `json:"ground_truth,omitempty"`
	Annotations map[string]json.RawMessage `json:"annotations,omitempty"`
	Timing      ObservationTiming          `json:"timing"`
}

// ObservationTiming captures latency and token usage for a single observation.
type ObservationTiming struct {
	TotalMs      int64   `json:"total_ms"`
	TTFTMs       int64   `json:"ttft_ms,omitempty"`
	TTLTMs       int64   `json:"ttlt_ms,omitempty"`
	MedianITL    float64 `json:"median_itl_ms,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
}

// Score is a single named metric value. If Error is non-empty, the scorer
// failed for this observation: Value is meaningless and the score is
// excluded from [Aggregate].
type Score struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Reason string  `json:"reason,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// ObservationResult pairs an observation with its scores.
type ObservationResult struct {
	Observation Observation `json:"observation"`
	Scores      []Score     `json:"scores"`
}

// SuiteResult is the complete output of an evaluation run.
type SuiteResult struct {
	Name      string              `json:"name"`
	CreatedAt time.Time           `json:"created_at"`
	Results   []ObservationResult `json:"results"`
	Aggregate map[string]float64  `json:"aggregate"`
	// ErroredCases counts observations with at least one errored score.
	ErroredCases int `json:"errored_cases,omitempty"`
}

// Run executes an evaluation suite: for each observation, it runs all scorers
// and collects results. Observations should have Output already populated
// (typically by a [Subject]).
//
// A scorer error does not abort the suite: it is recorded as a [Score] with
// its Error field set, excluded from [Aggregate], and counted in
// [SuiteResult.ErroredCases]. If scorers errored and no score succeeded
// anywhere in the suite, Run returns the suite result alongside a non-nil
// error so callers can still inspect per-case failures.
func Run(ctx context.Context, name string, observations []Observation, scorers []Scorer, opts ...Option) (*SuiteResult, error) {
	cfg := &Config{
		Concurrency: 1,
		Logger:      slog.Default(),
	}
	for _, o := range opts {
		o(cfg)
	}

	results := make([]ObservationResult, len(observations))

	sem := make(chan struct{}, cfg.Concurrency)

	var wg sync.WaitGroup
	for i, obs := range observations {
		wg.Add(1)
		go func(idx int, obs Observation) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}

			results[idx] = ObservationResult{
				Observation: obs,
				Scores:      scoreObservation(ctx, obs, scorers, cfg.Logger),
			}
		}(i, obs)
	}
	wg.Wait()

	erroredCases := 0
	succeeded := 0
	for _, r := range results {
		cerr := false
		for _, s := range r.Scores {
			if s.Error != "" {
				cerr = true
			} else if s.Name != "" {
				succeeded++
			}
		}
		if cerr {
			erroredCases++
		}
	}

	suite := &SuiteResult{
		Name:         name,
		CreatedAt:    time.Now(),
		Results:      results,
		Aggregate:    Aggregate(results),
		ErroredCases: erroredCases,
	}

	if erroredCases > 0 && succeeded == 0 {
		return suite, fmt.Errorf("eval suite %q: all %d observations with scores errored", name, erroredCases)
	}
	return suite, nil
}

// scoreObservation runs all scorers against a single observation. A scorer
// error is recorded as an errored [Score] and does not stop the remaining
// scorers.
func scoreObservation(ctx context.Context, obs Observation, scorers []Scorer, logger *slog.Logger) []Score {
	var scores []Score
	for _, s := range scorers {
		score, err := s.Score(ctx, obs)
		if err != nil {
			logger.Error("scorer failed", "observation", obs.ID, "scorer", s.Name(), "error", err)
			scores = append(scores, Score{Name: s.Name(), Error: err.Error()})
			continue
		}
		if score.Name != "" {
			scores = append(scores, score)
		}
	}
	return scores
}

// Populate runs a [Subject] against each observation, populating Output,
// Annotations, and Timing fields in place.
func Populate(ctx context.Context, observations []Observation, subject Subject) error {
	for i := range observations {
		if err := subject(ctx, &observations[i]); err != nil {
			return fmt.Errorf("observation %q: %w", observations[i].ID, err)
		}
	}
	return nil
}
