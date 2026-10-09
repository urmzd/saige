package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// RunRecord and Unit are the stored form of an evaluation, shared by every
// subsystem's evals and by the eval/store implementations. A [RunRecord] is one
// submitted evaluation with its lifecycle, gate outcome, and provenance. A
// [Unit] is one scored observation of that run, identified by a stable
// [UnitKey] so a rerun of the same case replaces it as a new attempt instead
// of adding a duplicate. [SuiteResult] remains the in-memory result of
// [Run]; [NewRunRecord] and [SuiteResult.Units] convert it, and [SuiteFromUnits]
// rebuilds it from storage so [SuiteResult.Check] and [CompareSuites] work on
// stored runs.

// RunStatus is the lifecycle of a [RunRecord]. It is separate from
// [RunRecord.Outcome]: a run can succeed and still fail its gates.
type RunStatus string

const (
	// RunRunning means units are still being recorded.
	RunRunning RunStatus = "running"
	// RunSucceeded means every observation was attempted. Scorer and
	// subject failures are counted on the run, not reflected here.
	RunSucceeded RunStatus = "succeeded"
	// RunErrored means the run stopped on an error.
	RunErrored RunStatus = "errored"
	// RunCanceled means the context ended before every observation was
	// scored, so the units cover only part of the dataset.
	RunCanceled RunStatus = "canceled"
)

// Finished reports whether the status is terminal.
func (s RunStatus) Finished() bool {
	return s == RunSucceeded || s == RunErrored || s == RunCanceled
}

// RunRecord is one stored evaluation run.
type RunRecord struct {
	ID    string `json:"id"`
	Suite string `json:"suite"`
	// Claim is the hypothesis of the experiment behind the run, if any.
	Claim string `json:"claim,omitempty"`
	// Revision identifies the configuration that ran, such as a content
	// hash of the dataset and scorers, so runs of the same suite with
	// different setups are not compared by accident.
	Revision string    `json:"revision,omitempty"`
	Status   RunStatus `json:"status"`
	// Outcome is the gate result, empty when the run was never gated.
	Outcome    Outcome     `json:"outcome,omitempty"`
	Violations []Violation `json:"violations,omitempty"`
	// Error is the run error for an errored or canceled run.
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt time.Time  `json:"finished_at,omitzero"`
	Provenance Provenance `json:"provenance,omitzero"`
	// Labels are run-level coordinates, such as the environment.
	Labels Labels `json:"labels,omitempty"`

	// Summary counts, copied from the [SuiteResult].
	Aggregate      map[string]float64 `json:"aggregate,omitempty"`
	Units          int                `json:"units"`
	ErroredCases   int                `json:"errored_cases,omitempty"`
	UnstableScores int                `json:"unstable_scores,omitempty"`
	SubjectErrors  int                `json:"subject_errors,omitempty"`
	Incomplete     int                `json:"incomplete,omitempty"`
	// CostUSD sums the recorded cost of the units; nil when no unit
	// recorded one.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// TraceRef links a unit to the trace that produced it.
type TraceRef struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id,omitempty"`
}

// Unit is one stored, scored observation of a [RunRecord].
type Unit struct {
	RunID string `json:"run_id"`
	// Key identifies the unit within the run; see [UnitKey].
	Key string `json:"key"`
	// Attempt numbers reruns of the same key from 1. A store assigns it.
	Attempt     int         `json:"attempt"`
	Observation Observation `json:"observation"`
	Scores      []Score     `json:"scores"`
	Trace       *TraceRef   `json:"trace,omitempty"`
	RecordedAt  time.Time   `json:"recorded_at,omitzero"`
}

// Result returns the unit as an [ObservationResult].
func (u Unit) Result() ObservationResult {
	return ObservationResult{Observation: u.Observation, Scores: u.Scores}
}

// UnitKey is the stable identity of an observation within a run: its ID and
// turn, then "#" and the sample number when replicated, then "@" and the
// variant label when it has one, as in "q7/0#2@candidate".
func UnitKey(obs Observation) string {
	key := obs.ID + "/" + strconv.Itoa(obs.Turn)
	if obs.Sample > 0 {
		key += "#" + strconv.Itoa(obs.Sample)
	}
	if v := obs.Labels[LabelVariant]; v != "" {
		key += "@" + v
	}
	return key
}

// NewRunID returns a new run ID that sorts by creation time, such as
// "20261009T141503Z-3f9a1c2b".
func NewRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// StatusOf maps the error returned by [Run] or [Experiment.Run] to a run
// status: nil is succeeded, a context cancellation or deadline is canceled,
// and anything else is errored.
func StatusOf(err error) RunStatus {
	switch {
	case err == nil:
		return RunSucceeded
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return RunCanceled
	default:
		return RunErrored
	}
}

// NewRunRecord summarizes a finished suite as a [RunRecord] with the given ID. runErr is
// the error the suite's run returned; it sets the status and Error. The
// provenance is copied, so later changes to prov do not reach the run.
func NewRunRecord(id string, suite *SuiteResult, runErr error, prov Provenance) RunRecord {
	run := RunRecord{
		ID:         id,
		Status:     StatusOf(runErr),
		FinishedAt: time.Now().UTC(),
		Provenance: cloneProvenance(prov),
	}
	if runErr != nil {
		run.Error = runErr.Error()
	}
	if suite == nil {
		run.StartedAt = run.FinishedAt
		return run
	}
	run.Suite = suite.Name
	run.Claim = suite.Claim
	run.StartedAt = suite.CreatedAt.UTC()
	run.Outcome = suite.Outcome
	run.Violations = append([]Violation(nil), suite.Violations...)
	run.Units = len(suite.Results)
	run.ErroredCases = suite.ErroredCases
	run.UnstableScores = suite.UnstableScores
	run.SubjectErrors = suite.SubjectErrors
	run.Incomplete = suite.Incomplete
	if len(suite.Aggregate) > 0 {
		run.Aggregate = make(map[string]float64, len(suite.Aggregate))
		for k, v := range suite.Aggregate {
			run.Aggregate[k] = v
		}
	}
	run.CostUSD = TotalCostUSD(suite.Results)
	return run
}

// Units converts the suite's results into units of the run with the given
// ID, keyed by [UnitKey]. Attempt is left zero for the store to assign.
func (s *SuiteResult) Units(runID string) []Unit {
	units := make([]Unit, len(s.Results))
	for i, r := range s.Results {
		units[i] = Unit{
			RunID:       runID,
			Key:         UnitKey(r.Observation),
			Observation: r.Observation,
			Scores:      r.Scores,
		}
	}
	return units
}

// SuiteFromUnits rebuilds a [SuiteResult] from a stored run and its units,
// recomputing the aggregate from the unit scores.
func SuiteFromUnits(run RunRecord, units []Unit) *SuiteResult {
	results := make([]ObservationResult, len(units))
	for i, u := range units {
		results[i] = u.Result()
	}
	return &SuiteResult{
		Name:           run.Suite,
		Claim:          run.Claim,
		CreatedAt:      run.StartedAt,
		Results:        results,
		Aggregate:      Aggregate(results),
		ErroredCases:   run.ErroredCases,
		UnstableScores: run.UnstableScores,
		SubjectErrors:  run.SubjectErrors,
		Incomplete:     run.Incomplete,
		Outcome:        run.Outcome,
		Violations:     append([]Violation(nil), run.Violations...),
	}
}

// TotalCostUSD sums the recorded cost of the results. It returns nil when no
// result recorded a cost, so an unpriced run is not reported as free.
func TotalCostUSD(results []ObservationResult) *float64 {
	var (
		total    float64
		recorded bool
	)
	for _, r := range results {
		if c := r.Observation.Timing.CostUSD; c != nil {
			total += *c
			recorded = true
		}
	}
	if !recorded {
		return nil
	}
	return &total
}

func cloneProvenance(p Provenance) Provenance {
	if p.Catalog != nil {
		c := *p.Catalog
		c.Presets = append([]string(nil), c.Presets...)
		c.Configs = make(map[string]string, len(p.Catalog.Configs))
		for k, v := range p.Catalog.Configs {
			c.Configs[k] = v
		}
		p.Catalog = &c
	}
	p.Models = append([]string(nil), p.Models...)
	if len(p.Models) == 0 {
		p.Models = nil
	}
	if p.Extra != nil {
		extra := make(map[string]string, len(p.Extra))
		for k, v := range p.Extra {
			extra[k] = v
		}
		p.Extra = extra
	}
	return p
}
