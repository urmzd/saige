// Package eval provides a universal evaluation framework for SAIGE subsystems.
//
// The framework is built on three abstractions:
//   - [Observation]: a universal eval case carrying typed I/O as JSON
//   - [Scorer]: an interface for computing a named metric from an Observation
//   - [Subject]: a function that populates an Observation's output and annotations
//
// On top of these, [Assertion] gates a suite, [Compare] and [CompareSuites]
// compare two arms case by case, [Experiment] runs labeled [Variant]s over a
// dataset, and [GroupBy] reduces results by [Labels]. The statistics behind
// them live in the eval/analysis package.
//
// Deterministic check scorers ([JSONSchemaScorer], [RegexCountScorer],
// [TokenBudgetScorer], and others) test output contracts without an LLM, and
// a [Registry] builds scorers from a [ScorerSpec] by kind. Results are stored
// as a [RunRecord] with one [Unit] per observation through the eval/store
// package, stamped with the [Provenance] of the run.
//
// Subsystem-specific scorers live next to their subsystems, in agent/eval,
// rag/eval, and rag/knowledge/eval, and operate on well-known annotation keys
// set by their respective subjects.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// AnnotationSubjectError is the annotation key [PopulateAll] sets, as a JSON
// string, when the subject failed for an observation. [Run] does not score
// such an observation and counts it in [SuiteResult.SubjectErrors].
const AnnotationSubjectError = "eval.subject_error"

// Observation is the universal eval case. Input, Output, and GroundTruth use
// json.RawMessage so the same structure works for RAG queries, agent
// conversations, KG episodes, or end-to-end flows.
type Observation struct {
	ID   string `json:"id"`
	Turn int    `json:"turn"`
	// Sample numbers the copies made by [Sampler.Replicate], from 1. Zero
	// means the observation was not replicated.
	Sample int `json:"sample,omitempty"`
	// Labels place the observation in the dataset and the experiment
	// matrix, such as {"topic": "billing", "variant": "gpt/rag"}. They are
	// what [Where], [GroupBy], and assertions select on.
	Labels      Labels                     `json:"labels,omitempty"`
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
	// CostUSD is what producing the output cost, in USD. Nil means the
	// subject did not record a cost, which is not the same as free.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// Score is a single named metric value. If Error is non-empty, the scorer
// failed for this observation: Value is meaningless and the score is
// excluded from [Aggregate].
type Score struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Reason string  `json:"reason,omitempty"`
	Error  string  `json:"error,omitempty"`
	// Samples is set when the score came from a [Sampled] scorer: Value is
	// then the reduced value and Samples describes the spread behind it.
	Samples *SampleStats `json:"samples,omitempty"`
	// Inconclusive is set with Error when the scorer failed on
	// infrastructure (see [IsInfra]), such as a judge whose provider was
	// down. Such a score says nothing about the output: gates report it as
	// inconclusive instead of failed and leave its Passed nil.
	Inconclusive bool `json:"inconclusive,omitempty"`
	// Passed is set by [SuiteResult.Gate] when a per-case [Assertion]
	// covers this score: true when every such assertion held, false when
	// one failed or the score errored. Nil means no gate applied, or an
	// inconclusive score.
	Passed *bool `json:"passed,omitempty"`
}

// ObservationResult pairs an observation with its scores.
type ObservationResult struct {
	Observation Observation `json:"observation"`
	Scores      []Score     `json:"scores"`
}

// SuiteResult is the complete output of an evaluation run.
type SuiteResult struct {
	Name string `json:"name"`
	// Claim is the hypothesis of the [Experiment] that produced the suite.
	Claim     string              `json:"claim,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	Results   []ObservationResult `json:"results"`
	Aggregate map[string]float64  `json:"aggregate"`
	// ErroredCases counts observations with at least one errored score.
	ErroredCases int `json:"errored_cases,omitempty"`
	// UnstableScores counts sampled scores whose spread exceeded the
	// Sampler's tolerance, the verdicts that changed between samples, and
	// scores whose samples reported themselves unstable, such as a pairwise
	// judge that contradicted itself across the position swap.
	UnstableScores int `json:"unstable_scores,omitempty"`
	// SubjectErrors counts observations whose subject failed (see
	// [AnnotationSubjectError]). They appear in Results with no scores.
	// Failures on infrastructure are counted here and in Inconclusive.
	SubjectErrors int `json:"subject_errors,omitempty"`
	// Inconclusive counts results that could not be measured (see
	// [ObservationResult.Inconclusive]): the subject or a scorer failed on
	// infrastructure. They are left out of pass rates and aggregates.
	Inconclusive int `json:"inconclusive,omitempty"`
	// Incomplete counts observations that were not scored because the
	// context ended first. They are left out of Results, and Run returns
	// the context's error alongside the partial suite.
	Incomplete int `json:"incomplete,omitempty"`
	// Outcome is the gate result set by [SuiteResult.Gate]; empty when the
	// suite was never gated.
	Outcome Outcome `json:"outcome,omitempty"`
	// Violations lists the failed gates behind a failed or inconclusive
	// Outcome. A passed outcome can still list inconclusive violations
	// within the [GatePolicy] tolerance.
	Violations []Violation `json:"violations,omitempty"`
}

// Run executes an evaluation suite: for each observation, it runs all scorers
// and collects results. Observations should have Output already populated
// (typically by a [Subject]).
//
// A scorer error does not abort the suite: it is recorded as a [Score] with
// its Error field set (and Inconclusive when [IsInfra] reports the error),
// excluded from [Aggregate], and counted in [SuiteResult.ErroredCases]. If scorers errored and no score succeeded
// anywhere in the suite, Run returns the suite result alongside a non-nil
// error so callers can still inspect per-case failures.
//
// With [WithAssertions], the suite is gated after scoring; a failed gate sets
// [SuiteResult.Outcome] and is not an error.
//
// If ctx ends before every observation is scored, Run returns the partial
// suite with an error wrapping ctx.Err(). Unscored observations, including
// those whose scorer was cut off by the cancellation, are left out of
// Results and counted in [SuiteResult.Incomplete].
func Run(ctx context.Context, name string, observations []Observation, scorers []Scorer, opts ...Option) (*SuiteResult, error) {
	cfg := newConfig(opts)

	if cfg.Sampler.N > 1 {
		sampled := make([]Scorer, len(scorers))
		for i, sc := range scorers {
			sampled[i] = Sampled(sc, cfg.Sampler)
		}
		scorers = sampled
	}

	var results []ObservationResult
	var done []bool
	if cfg.batch != nil {
		results, done = make([]ObservationResult, len(observations)), make([]bool, len(observations))
		runTasks(ctx, cfg.batch, scoreTasks("", observations, scorers, cfg.Logger, results, done))
	} else {
		results, done = scoreAll(ctx, cfg, observations, scorers)
	}
	return suiteFrom(ctx, name, cfg, observations, results, done)
}

// scoreAll scores every observation, up to cfg.Concurrency at once.
func scoreAll(ctx context.Context, cfg *Config, observations []Observation, scorers []Scorer) ([]ObservationResult, []bool) {
	results := make([]ObservationResult, len(observations))
	done := make([]bool, len(observations))

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
launch:
	for i, obs := range observations {
		// Take the slot before starting the goroutine so a large dataset
		// does not park one goroutine per observation.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break launch
		}
		if ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(idx int, obs Observation) {
			defer wg.Done()
			defer func() { <-sem }()

			if SubjectError(obs) != "" {
				results[idx] = ObservationResult{Observation: obs}
				done[idx] = true
				return
			}
			scores, interrupted := scoreObservation(ctx, obs, scorers, cfg.Logger)
			if interrupted {
				return
			}
			results[idx] = ObservationResult{Observation: obs, Scores: scores}
			done[idx] = true
		}(i, obs)
	}
	wg.Wait()

	return results, done
}

// suiteFrom builds the suite from the scored observations, gates it and
// reports incomplete or failed runs.
func suiteFrom(ctx context.Context, name string, cfg *Config, observations []Observation, results []ObservationResult, done []bool) (*SuiteResult, error) {
	completed := make([]ObservationResult, 0, len(results))
	for i, r := range results {
		if done[i] {
			completed = append(completed, r)
		}
	}

	erroredCases := 0
	succeeded := 0
	unstable := 0
	subjectErrors := 0
	for _, r := range completed {
		if SubjectError(r.Observation) != "" {
			subjectErrors++
			continue
		}
		cerr := false
		for _, s := range r.Scores {
			if s.Samples != nil && !s.Samples.Stable {
				unstable++
			}
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
		Name:           name,
		CreatedAt:      time.Now(),
		Results:        completed,
		Aggregate:      Aggregate(completed),
		ErroredCases:   erroredCases,
		UnstableScores: unstable,
		SubjectErrors:  subjectErrors,
		Incomplete:     len(observations) - len(completed),
		Inconclusive:   countInconclusive(completed),
	}
	if len(cfg.Assertions) > 0 {
		suite.GateWith(cfg.GatePolicy, cfg.Assertions...)
	}

	if suite.Incomplete > 0 {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return suite, fmt.Errorf("eval suite %q: %d of %d observations incomplete: %w",
			name, suite.Incomplete, len(observations), cause)
	}
	if erroredCases > 0 && succeeded == 0 {
		return suite, fmt.Errorf("eval suite %q: all %d observations with scores errored", name, erroredCases)
	}
	if subjectErrors > 0 && subjectErrors == len(observations) {
		return suite, fmt.Errorf("eval suite %q: subject failed on all %d observations", name, subjectErrors)
	}
	return suite, nil
}

// scoreObservation runs all scorers against a single observation. A scorer
// error is recorded as an errored [Score] and does not stop the remaining
// scorers. interrupted reports that ctx ended while scoring, so the scores
// are incomplete and must not be reported as a finished case.
func scoreObservation(ctx context.Context, obs Observation, scorers []Scorer, logger *slog.Logger) (scores []Score, interrupted bool) {
	for _, s := range scorers {
		if ctx.Err() != nil {
			return nil, true
		}
		score, err := s.Score(ctx, obs)
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil, true
			}
			logger.Error("scorer failed", "observation", obs.ID, "scorer", s.Name(), "error", err)
			scores = append(scores, Score{Name: s.Name(), Error: err.Error(), Inconclusive: IsInfra(err)})
			continue
		}
		if score.Name != "" {
			scores = append(scores, score)
		}
	}
	return scores, false
}

// Populate runs a [Subject] against each observation, populating Output,
// Annotations, and Timing fields in place. It runs one observation at a time
// and stops at the first subject error. Use [PopulateAll] to keep going.
func Populate(ctx context.Context, observations []Observation, subject Subject) error {
	for i := range observations {
		if err := subject(ctx, &observations[i]); err != nil {
			return fmt.Errorf("observation %q: %w", observations[i].ID, err)
		}
	}
	return nil
}

// PopulateAll runs a [Subject] against every observation, up to the
// [WithConcurrency] limit at once. A subject error does not stop the others:
// it is recorded on that observation under [AnnotationSubjectError] (with
// [AnnotationSubjectInconclusive] when [IsInfra] reports it), and [Run]
// reports the observation without scoring it. PopulateAll returns the
// joined subject errors, or nil when every subject succeeded. If ctx ends,
// observations not yet started are marked with the context's error.
func PopulateAll(ctx context.Context, observations []Observation, subject Subject, opts ...Option) error {
	cfg := newConfig(opts)
	if cfg.dialPolicy != nil {
		ctx = types.ContextWithDialPolicy(ctx, *cfg.dialPolicy)
	}
	errs := make([]error, len(observations))
	if cfg.batch != nil {
		runTasks(ctx, cfg.batch, populateTasks("", observations, subject, errs))
		return finishPopulate(cfg, observations, errs)
	}

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i := range observations {
		acquired := false
		select {
		case sem <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			if acquired {
				<-sem
			}
			errs[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = subject(ctx, &observations[i])
		}(i)
	}
	wg.Wait()
	return finishPopulate(cfg, observations, errs)
}

// finishPopulate records each subject error on its observation and joins
// them.
func finishPopulate(cfg *Config, observations []Observation, errs []error) error {
	var joined []error
	for i, err := range errs {
		obs := &observations[i]
		MarkSubjectError(obs, err)
		if err == nil {
			continue
		}
		cfg.Logger.Error("subject failed", "observation", obs.ID, "error", err)
		joined = append(joined, fmt.Errorf("observation %q: %w", obs.ID, err))
	}
	return errors.Join(joined...)
}

// SubjectError returns the subject error [PopulateAll] recorded on obs, or
// "" when there is none.
func SubjectError(obs Observation) string {
	raw, ok := obs.Annotations[AnnotationSubjectError]
	if !ok {
		return ""
	}
	var msg string
	if err := json.Unmarshal(raw, &msg); err != nil {
		return string(raw)
	}
	return msg
}
