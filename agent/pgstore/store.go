// Package pgstore implements agent/types.Store using PostgreSQL.
package pgstore

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/types"
)

var _ types.Store = (*Store)(nil)

// Store implements types.Store backed by PostgreSQL.
// The pool should already be connected; schema migration is handled
// separately via postgres.RunMigrations.
type Store struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewStore creates a new PostgreSQL-backed agent store.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{pool: pool, logger: logger}
}
