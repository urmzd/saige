package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// Queries beyond store.Store. Each answers a question the generic interface
// can only answer by loading every unit of every run, and each is backed by
// an index: the history of one case across runs, and a score-by-score diff
// of two runs for regression checks. The baseline of a suite is
// store.LatestSucceeded, which runs on the (suite, status, started_at)
// index.

// CaseEntry is one stored attempt of a case in [Store.CaseHistory].
type CaseEntry struct {
	RunID     string    `json:"run_id"`
	Suite     string    `json:"suite"`
	StartedAt time.Time `json:"started_at"`
	Unit      eval.Unit `json:"unit"`
}

// CaseHistory returns the current unit of observationID in each run that
// recorded it, newest run first. suite, when set, keeps runs of one suite.
// limit caps the result; zero means no limit.
func (s *Store) CaseHistory(ctx context.Context, suite, observationID string, limit int) ([]CaseEntry, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.suite, r.started_at, u.unit
		FROM eval_unit u
		JOIN eval_run r ON r.tenant = u.tenant AND r.id = u.run_id
		WHERE u.tenant = $1 AND u.observation_id = $2
		  AND ($3 = '' OR r.suite = $3)
		ORDER BY r.started_at DESC, r.id COLLATE "C" DESC, u.seq
		LIMIT $4`, s.tenant, observationID, suite, lim)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: case history: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CaseEntry, error) {
		var (
			e   CaseEntry
			raw []byte
		)
		if err := row.Scan(&e.RunID, &e.Suite, &e.StartedAt, &raw); err != nil {
			return e, err
		}
		if err := json.Unmarshal(raw, &e.Unit); err != nil {
			return e, fmt.Errorf("decode unit: %w", err)
		}
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: case history: %w", err)
	}
	return entries, nil
}

// ScoreDiff is one metric of one unit key compared across two runs. A side
// that did not record the metric is nil.
type ScoreDiff struct {
	Key    string `json:"key"`
	Metric string `json:"metric"`
	// Base and Candidate are the score values, nil when that run has no
	// such score for the key.
	Base      *float64 `json:"base,omitempty"`
	Candidate *float64 `json:"candidate,omitempty"`
	// BaseErrored and CandidateErrored mark a score whose scorer failed;
	// its value is meaningless.
	BaseErrored      bool `json:"base_errored,omitempty"`
	CandidateErrored bool `json:"candidate_errored,omitempty"`
	// BaseInconclusive and CandidateInconclusive mark an errored score
	// whose scorer failed on infrastructure (see [eval.Score.Inconclusive]),
	// which says nothing about the output. CandidateInconclusive is also
	// set for a score missing from the candidate because its subject failed
	// on infrastructure for that case (see [eval.SubjectInconclusive]).
	BaseInconclusive      bool `json:"base_inconclusive,omitempty"`
	CandidateInconclusive bool `json:"candidate_inconclusive,omitempty"`
}

// Delta is Candidate minus Base. ok is false when either side is missing or
// errored.
func (d ScoreDiff) Delta() (delta float64, ok bool) {
	if d.Base == nil || d.Candidate == nil || d.BaseErrored || d.CandidateErrored {
		return 0, false
	}
	return *d.Candidate - *d.Base, true
}

// DiffFilter narrows [Store.Diff].
type DiffFilter struct {
	// Metric keeps one metric; empty keeps all.
	Metric string
	// Regressions keeps only scores that dropped by more than MinDrop, plus
	// keys whose candidate score errored or went missing. A candidate score
	// that is inconclusive (see ScoreDiff.CandidateInconclusive) is not a
	// regression and is left out.
	Regressions bool
	// MinDrop is the smallest drop counted as a regression.
	MinDrop float64
}

// Diff compares the current scores of two runs key by key and metric by
// metric, the largest drop first. Both runs must exist.
func (s *Store) Diff(ctx context.Context, baseRunID, candidateRunID string, filter DiffFilter) ([]ScoreDiff, error) {
	for _, id := range []string{baseRunID, candidateRunID} {
		if err := s.requireRun(ctx, id); err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `
		WITH b AS (
		    SELECT s.key, s.name, s.value, s.errored, s.inconclusive, u.observation_id
		    FROM eval_score s
		    JOIN eval_unit u ON u.tenant = s.tenant AND u.run_id = s.run_id AND u.key = s.key
		    WHERE s.tenant = $1 AND s.run_id = $2 AND ($4 = '' OR s.name = $4)
		), c AS (
		    SELECT key, name, value, errored, inconclusive FROM eval_score
		    WHERE tenant = $1 AND run_id = $3 AND ($4 = '' OR name = $4)
		), unmeasured AS (
		    SELECT DISTINCT observation_id FROM eval_unit
		    WHERE tenant = $1 AND run_id = $3
		      AND unit->'observation'->'annotations'->'eval.subject_inconclusive' = 'true'::jsonb
		), j AS (
		    SELECT coalesce(b.key, c.key) AS key, coalesce(b.name, c.name) AS name,
		           b.key IS NULL AS b_missing, c.key IS NULL AS c_missing,
		           b.value AS b_value, coalesce(b.errored, false) AS b_errored,
		           coalesce(b.inconclusive, false) AS b_inconclusive,
		           c.value AS c_value, coalesce(c.errored, false) AS c_errored,
		           coalesce(c.inconclusive, false)
		               OR (c.key IS NULL AND b.observation_id IN (SELECT observation_id FROM unmeasured)) AS c_inconclusive
		    FROM b FULL JOIN c ON b.key = c.key AND b.name = c.name
		)
		SELECT key, name, b_value, b_errored, b_inconclusive, c_value, c_errored, c_inconclusive
		FROM j
		WHERE NOT $5::bool
		   OR (NOT b_missing AND NOT b_errored AND NOT c_inconclusive AND (
		          c_missing OR c_errored OR c_value < b_value - $6))
		ORDER BY CASE WHEN b_errored OR c_errored THEN NULL ELSE c_value - b_value END ASC NULLS FIRST,
		         name COLLATE "C", key COLLATE "C"`,
		s.tenant, baseRunID, candidateRunID, filter.Metric, filter.Regressions, filter.MinDrop)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: diff: %w", err)
	}
	diffs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ScoreDiff, error) {
		var d ScoreDiff
		err := row.Scan(&d.Key, &d.Metric, &d.Base, &d.BaseErrored, &d.BaseInconclusive,
			&d.Candidate, &d.CandidateErrored, &d.CandidateInconclusive)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: diff: %w", err)
	}
	return diffs, nil
}

// Baseline returns the newest succeeded run of suite. It is
// store.LatestSucceeded, named here for discoverability.
func (s *Store) Baseline(ctx context.Context, suite string) (eval.RunRecord, error) {
	return store.LatestSucceeded(ctx, s, suite)
}
