// Package pgstore implements knowledge/types.Store using PostgreSQL + pgvector.
package pgstore

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/knowledge/types"
)

var (
	_ types.Store            = (*Store)(nil)
	_ types.GroupScopedStore = (*Store)(nil)
	_ types.EpisodeDeleter   = (*Store)(nil)
)

// Store implements types.Store backed by PostgreSQL with pgvector.
type Store struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewStore creates a new PostgreSQL-backed knowledge store.
// The pool should already be connected; schema migration is handled separately via postgres.RunMigrations.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{pool: pool, logger: logger}
}
