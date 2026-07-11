// Package pgstore implements agent/types.Store using PostgreSQL.
package pgstore

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/types"
)

var _ types.Store = (*Store)(nil)

// Store implements types.Store backed by PostgreSQL.
//
// A Store is scoped to a single conversation: branch and checkpoint rows are
// namespaced by conversationID so that every conversation gets its own branch
// map (each tree starts with a "main" branch, and without namespacing two
// persisted conversations would overwrite each other's tips). Create one Store
// per conversation, mirroring how one memstore instance backs one tree. The
// root node ID of the tree is a natural choice of conversation ID.
//
// The pool should already be connected; schema migration is handled
// separately via postgres.RunMigrations.
type Store struct {
	pool           *pgxpool.Pool
	conversationID string
	logger         *slog.Logger
}

// NewStore creates a PostgreSQL-backed agent store scoped to conversationID.
// All branch and checkpoint operations are isolated to that namespace, so two
// conversations can each have a "main" branch without touching each other.
// An empty conversationID selects the legacy unscoped namespace (rows written
// before namespacing existed) and should only be used by single-conversation
// deployments.
func NewStore(pool *pgxpool.Pool, conversationID string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{pool: pool, conversationID: conversationID, logger: logger}
}
