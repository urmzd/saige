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
	// keys whose candidate score errored or went missing.
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
		    SELECT key, name, value, errored FROM eval_score
		    WHERE tenant = $1 AND run_id = $2 AND ($4 = '' OR name = $4)
		), c AS (
		    SELECT key, name, value, errored FROM eval_score
		    WHERE tenant = $1 AND run_id = $3 AND ($4 = '' OR name = $4)
		)
		SELECT coalesce(b.key, c.key), coalesce(b.name, c.name),
		       b.value, coalesce(b.errored, false), c.value, coalesce(c.errored, false)
		FROM b FULL JOIN c ON b.key = c.key AND b.name = c.name
		WHERE NOT $5::bool
		   OR (b.key IS NOT NULL AND NOT b.errored AND (
		          c.key IS NULL OR c.errored OR c.value < b.value - $6))
		ORDER BY CASE WHEN b.errored OR c.errored THEN NULL ELSE c.value - b.value END ASC NULLS FIRST,
		         coalesce(b.name, c.name) COLLATE "C", coalesce(b.key, c.key) COLLATE "C"`,
		s.tenant, baseRunID, candidateRunID, filter.Metric, filter.Regressions, filter.MinDrop)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: diff: %w", err)
	}
	diffs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ScoreDiff, error) {
		var d ScoreDiff
		err := row.Scan(&d.Key, &d.Metric, &d.Base, &d.BaseErrored, &d.Candidate, &d.CandidateErrored)
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
