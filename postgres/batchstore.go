package postgres

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/batch"
)

// batchSQL creates the table behind BatchStore. RunMigrations runs it.
//
//go:embed sql/batch.sql
var batchSQL string

// BatchStore is a batch.Store in the saige_batch_jobs table, so batch jobs
// survive restarts and several processes can share them. Each record is
// kept whole as JSONB, with the state and vendor batch ID in columns for
// listing. Update compares the version in the same statement, so two
// processes cannot both win a race. RunMigrations creates the table.
type BatchStore struct {
	pool *pgxpool.Pool
}

var _ batch.Store = (*BatchStore)(nil)

// NewBatchStore returns a batch job store on pool.
func NewBatchStore(pool *pgxpool.Pool) *BatchStore { return &BatchStore{pool: pool} }

func batchID(j batch.Job) string {
	if j.Handle == nil {
		return ""
	}
	return j.Handle.ID
}

// Create implements batch.Store.
func (s *BatchStore) Create(ctx context.Context, j batch.Job) (batch.Job, bool, error) {
	j.Version = 1
	raw, err := json.Marshal(j)
	if err != nil {
		return batch.Job{}, false, err
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO saige_batch_jobs (id, provider, model, manifest, state, batch_id, owner, record, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (id) DO NOTHING`,
		j.ID, j.Provider, j.Model, j.Manifest, string(j.State), batchID(j), j.Owner, raw, j.Version, j.CreatedAt, j.UpdatedAt)
	if err != nil {
		return batch.Job{}, false, fmt.Errorf("batch store: create: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return j, true, nil
	}
	cur, err := s.Get(ctx, j.ID)
	return cur, false, err
}

// Get implements batch.Store.
func (s *BatchStore) Get(ctx context.Context, id string) (batch.Job, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT record FROM saige_batch_jobs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return batch.Job{}, batch.ErrJobNotFound
	}
	if err != nil {
		return batch.Job{}, fmt.Errorf("batch store: get: %w", err)
	}
	var j batch.Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return batch.Job{}, fmt.Errorf("batch store: decode %s: %w", id, err)
	}
	return j, nil
}

// Update implements batch.Store.
func (s *BatchStore) Update(ctx context.Context, j batch.Job) (batch.Job, error) {
	prev := j.Version
	j.Version++
	raw, err := json.Marshal(j)
	if err != nil {
		return batch.Job{}, err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE saige_batch_jobs
		 SET state = $2, batch_id = $3, owner = $4, record = $5, version = $6, updated_at = $7
		 WHERE id = $1 AND version = $8`,
		j.ID, string(j.State), batchID(j), j.Owner, raw, j.Version, j.UpdatedAt, prev)
	if err != nil {
		return batch.Job{}, fmt.Errorf("batch store: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, j.ID); err != nil {
			return batch.Job{}, err
		}
		return batch.Job{}, batch.ErrJobConflict
	}
	return j, nil
}

// List implements batch.Store.
func (s *BatchStore) List(ctx context.Context, states ...batch.JobState) ([]batch.Job, error) {
	names := make([]string, len(states))
	for i, st := range states {
		names[i] = string(st)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT record FROM saige_batch_jobs
		 WHERE cardinality($1::text[]) = 0 OR state = ANY($1)
		 ORDER BY created_at, id`, names)
	if err != nil {
		return nil, fmt.Errorf("batch store: list: %w", err)
	}
	defer rows.Close()
	var out []batch.Job
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("batch store: list: %w", err)
		}
		var j batch.Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("batch store: list: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
