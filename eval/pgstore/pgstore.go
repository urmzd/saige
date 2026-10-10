// Package pgstore is an eval/store.Store on PostgreSQL.
//
// Runs live in eval_run, the current attempt of each unit in eval_unit,
// archived attempts in eval_unit_attempt, and one row per score of each
// current unit in eval_score. postgres.RunMigrations creates the tables and
// adds columns introduced later, such as eval_score.inconclusive, to tables
// an older release created; rows written before a column existed read as its
// default. The
// full run and unit records are kept as JSON, so every read returns exactly
// what was written; the other columns exist to index the queries a results
// store answers: runs newest first by suite and status, units of a run by
// label, the history of one case across runs, and a metric-by-metric diff of
// two runs.
//
// A Store is scoped to one tenant. Every row carries the tenant, every key
// includes it, and every query filters on it, so two tenants can use the same
// run IDs without seeing each other's runs. The empty tenant is the default
// scope of a single-tenant deployment.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// ErrSchemaMissing reports a database without the eval tables. Run
// postgres.RunMigrations first.
var ErrSchemaMissing = errors.New("eval pgstore: eval tables missing; run postgres.RunMigrations")

// Store is a tenant-scoped eval/store.Store on PostgreSQL. It is safe for
// concurrent use, including from several processes sharing one database.
type Store struct {
	pool   *pgxpool.Pool
	tenant string
}

var _ store.Store = (*Store)(nil)

// New returns a store for tenant on pool. It checks that the eval tables
// exist and returns ErrSchemaMissing when they do not. Migration is
// separate: call postgres.RunMigrations, which also checks the server
// version, before New.
func New(ctx context.Context, pool *pgxpool.Pool, tenant string) (*Store, error) {
	var missing int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM unnest($1::text[]) AS t(name) WHERE to_regclass(t.name) IS NULL`,
		[]string{"eval_run", "eval_unit", "eval_unit_attempt", "eval_score"}).Scan(&missing)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: check schema: %w", err)
	}
	if missing > 0 {
		return nil, ErrSchemaMissing
	}
	return &Store{pool: pool, tenant: tenant}, nil
}

// Tenant returns the scope the store reads and writes.
func (s *Store) Tenant() string { return s.tenant }

// CreateRun implements store.Store.
func (s *Store) CreateRun(ctx context.Context, run eval.RunRecord) error {
	if err := store.ValidateRunID(run.ID); err != nil {
		return err
	}
	args, err := runArgs(run)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO eval_run (tenant, id, suite, status, revision, labels, started_at, finished_at, record)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		append([]any{s.tenant}, args...)...)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %q", store.ErrRunExists, run.ID)
	}
	if err != nil {
		return fmt.Errorf("eval pgstore: create run: %w", err)
	}
	return nil
}

// UpdateRun implements store.Store.
func (s *Store) UpdateRun(ctx context.Context, run eval.RunRecord) error {
	args, err := runArgs(run)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE eval_run SET suite = $3, status = $4, revision = $5, labels = $6,
		       started_at = $7, finished_at = $8, record = $9
		WHERE tenant = $1 AND id = $2`,
		append([]any{s.tenant}, args...)...)
	if err != nil {
		return fmt.Errorf("eval pgstore: update run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %q", store.ErrNotFound, run.ID)
	}
	return nil
}

// runArgs returns the column values of run after the tenant.
func runArgs(run eval.RunRecord) ([]any, error) {
	record, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: encode run: %w", err)
	}
	labels, err := labelsJSON(run.Labels)
	if err != nil {
		return nil, err
	}
	var finished *time.Time
	if !run.FinishedAt.IsZero() {
		finished = &run.FinishedAt
	}
	return []any{run.ID, run.Suite, string(run.Status), run.Revision, labels, run.StartedAt, finished, record}, nil
}

func labelsJSON(l eval.Labels) ([]byte, error) {
	if len(l) == 0 {
		return []byte(`{}`), nil
	}
	out, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: encode labels: %w", err)
	}
	return out, nil
}

// GetRun implements store.Store.
func (s *Store) GetRun(ctx context.Context, id string) (eval.RunRecord, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT record FROM eval_run WHERE tenant = $1 AND id = $2`, s.tenant, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return eval.RunRecord{}, fmt.Errorf("%w: run %q", store.ErrNotFound, id)
	}
	if err != nil {
		return eval.RunRecord{}, fmt.Errorf("eval pgstore: get run: %w", err)
	}
	return decodeRun(raw)
}

func decodeRun(raw []byte) (eval.RunRecord, error) {
	var run eval.RunRecord
	if err := json.Unmarshal(raw, &run); err != nil {
		return eval.RunRecord{}, fmt.Errorf("eval pgstore: decode run: %w", err)
	}
	return run, nil
}

// ListRuns implements store.Store. Runs are ordered newest StartedAt first,
// ties broken by ID, as [store.SortRuns] orders them.
func (s *Store) ListRuns(ctx context.Context, filter store.RunFilter) ([]eval.RunRecord, error) {
	statuses := make([]string, len(filter.Status))
	for i, st := range filter.Status {
		statuses[i] = string(st)
	}
	var limit *int
	if filter.Limit > 0 {
		limit = &filter.Limit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT record FROM eval_run
		WHERE tenant = $1
		  AND ($2 = '' OR suite = $2)
		  AND (cardinality($3::text[]) = 0 OR status = ANY($3))
		ORDER BY started_at DESC, id COLLATE "C" DESC
		LIMIT $4`, s.tenant, filter.Suite, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: list runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (eval.RunRecord, error) {
		var raw []byte
		if err := row.Scan(&raw); err != nil {
			return eval.RunRecord{}, err
		}
		return decodeRun(raw)
	})
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: list runs: %w", err)
	}
	// The database holds microseconds; the Go sort breaks any tie the
	// rounding made the same way every other store does.
	return store.SortRuns(runs, 0), nil
}

// PutUnit implements store.Store. Puts to one run are serialized by a lock
// on the run's row, so attempt numbers stay dense under concurrent writers.
func (s *Store) PutUnit(ctx context.Context, unit eval.Unit) (eval.Unit, error) {
	if unit.Key == "" {
		unit.Key = eval.UnitKey(unit.Observation)
	}
	if unit.RecordedAt.IsZero() {
		unit.RecordedAt = time.Now().UTC()
	}
	var out eval.Unit
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM eval_run WHERE tenant = $1 AND id = $2 FOR NO KEY UPDATE`,
			s.tenant, unit.RunID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: run %q", store.ErrNotFound, unit.RunID)
		}
		if err != nil {
			return err
		}

		var (
			priorAttempt  int
			priorRecorded time.Time
			prior         []byte
		)
		err = tx.QueryRow(ctx, `SELECT attempt, recorded_at, unit FROM eval_unit WHERE tenant = $1 AND run_id = $2 AND key = $3`,
			s.tenant, unit.RunID, unit.Key).Scan(&priorAttempt, &priorRecorded, &prior)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		unit.Attempt = 1
		if exists {
			unit.Attempt = priorAttempt + 1
			if _, err := tx.Exec(ctx, `
				INSERT INTO eval_unit_attempt (tenant, run_id, key, attempt, recorded_at, unit)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				s.tenant, unit.RunID, unit.Key, priorAttempt, priorRecorded, prior); err != nil {
				return err
			}
		}

		data, err := json.Marshal(unit)
		if err != nil {
			return fmt.Errorf("encode unit: %w", err)
		}
		labels, err := labelsJSON(unit.Observation.Labels)
		if err != nil {
			return err
		}
		if exists {
			_, err = tx.Exec(ctx, `
				UPDATE eval_unit SET attempt = $4, observation_id = $5, labels = $6, recorded_at = $7, unit = $8
				WHERE tenant = $1 AND run_id = $2 AND key = $3`,
				s.tenant, unit.RunID, unit.Key, unit.Attempt, unit.Observation.ID, labels, unit.RecordedAt, data)
			if err == nil {
				_, err = tx.Exec(ctx, `DELETE FROM eval_score WHERE tenant = $1 AND run_id = $2 AND key = $3`,
					s.tenant, unit.RunID, unit.Key)
			}
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO eval_unit (tenant, run_id, key, attempt, observation_id, labels, recorded_at, unit)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				s.tenant, unit.RunID, unit.Key, unit.Attempt, unit.Observation.ID, labels, unit.RecordedAt, data)
		}
		if err != nil {
			return err
		}
		if err := insertScores(ctx, tx, s.tenant, unit); err != nil {
			return err
		}
		return json.Unmarshal(data, &out)
	})
	if errors.Is(err, store.ErrNotFound) {
		return eval.Unit{}, err
	}
	if err != nil {
		return eval.Unit{}, fmt.Errorf("eval pgstore: put unit %q: %w", unit.Key, err)
	}
	return out, nil
}

// insertScores writes one eval_score row per named score of unit. When a
// unit carries two scores with the same name, the first is indexed; both
// stay in the unit record.
func insertScores(ctx context.Context, tx pgx.Tx, tenant string, unit eval.Unit) error {
	batch := &pgx.Batch{}
	for _, sc := range unit.Scores {
		if sc.Name == "" {
			continue
		}
		batch.Queue(`
			INSERT INTO eval_score (tenant, run_id, key, name, value, errored, passed, inconclusive)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT DO NOTHING`,
			tenant, unit.RunID, unit.Key, sc.Name, sc.Value, sc.Error != "", sc.Passed, sc.Inconclusive)
	}
	if batch.Len() == 0 {
		return nil
	}
	return tx.SendBatch(ctx, batch).Close()
}

// Units implements store.Store. The Where filter runs in the database as a
// JSON containment test on the observation labels.
func (s *Store) Units(ctx context.Context, runID string, filter store.UnitFilter) ([]eval.Unit, error) {
	if err := s.requireRun(ctx, runID); err != nil {
		return nil, err
	}
	where, err := labelsJSON(eval.Labels(filter.Where))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT unit FROM eval_unit
		WHERE tenant = $1 AND run_id = $2
		  AND labels @> $3
		  AND ($4 = '' OR observation_id = $4)
		ORDER BY seq`, s.tenant, runID, where, filter.ObservationID)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: units: %w", err)
	}
	return collectUnits(rows)
}

// Attempts implements store.Store.
func (s *Store) Attempts(ctx context.Context, runID, key string) ([]eval.Unit, error) {
	if err := s.requireRun(ctx, runID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT unit FROM eval_unit_attempt
		WHERE tenant = $1 AND run_id = $2 AND key = $3
		ORDER BY attempt`, s.tenant, runID, key)
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: attempts: %w", err)
	}
	return collectUnits(rows)
}

func (s *Store) requireRun(ctx context.Context, runID string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM eval_run WHERE tenant = $1 AND id = $2`, s.tenant, runID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: run %q", store.ErrNotFound, runID)
	}
	if err != nil {
		return fmt.Errorf("eval pgstore: get run: %w", err)
	}
	return nil
}

func collectUnits(rows pgx.Rows) ([]eval.Unit, error) {
	units, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (eval.Unit, error) {
		var raw []byte
		if err := row.Scan(&raw); err != nil {
			return eval.Unit{}, err
		}
		var u eval.Unit
		if err := json.Unmarshal(raw, &u); err != nil {
			return eval.Unit{}, fmt.Errorf("decode unit: %w", err)
		}
		return u, nil
	})
	if err != nil {
		return nil, fmt.Errorf("eval pgstore: units: %w", err)
	}
	return units, nil
}
