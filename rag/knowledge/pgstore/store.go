// Package pgstore implements knowledge/types.Store using PostgreSQL + pgvector.
package pgstore

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/rag/knowledge/types"
)

var (
	_ types.Store                  = (*Store)(nil)
	_ types.GroupScopedStore       = (*Store)(nil)
	_ types.EpisodeDeleter         = (*Store)(nil)
	_ types.DocumentEpisodeDeleter = (*Store)(nil)
	_ types.EpisodeLinker          = (*Store)(nil)
)

const (
	// DefaultMaxNodes is the default cap on the nodes GetNode returns.
	DefaultMaxNodes = 1000
	// DefaultMaxEdges is the default cap on the edges GetNode returns.
	DefaultMaxEdges = 5000
)

// Store implements types.Store backed by PostgreSQL with pgvector.
type Store struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	maxNodes int
	maxEdges int
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithTraversalLimits caps the neighbors and edges one GetNode call
// returns. A traversal that reaches either cap stops and reports
// NodeDetail.Truncated. Values of zero or less keep the defaults
// (DefaultMaxNodes, DefaultMaxEdges).
func WithTraversalLimits(maxNodes, maxEdges int) StoreOption {
	return func(s *Store) {
		if maxNodes > 0 {
			s.maxNodes = maxNodes
		}
		if maxEdges > 0 {
			s.maxEdges = maxEdges
		}
	}
}

// NewStore creates a new PostgreSQL-backed knowledge store.
// The pool should already be connected; schema migration is handled separately via postgres.RunMigrations.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger, opts ...StoreOption) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{pool: pool, logger: logger, maxNodes: DefaultMaxNodes, maxEdges: DefaultMaxEdges}
	for _, o := range opts {
		o(s)
	}
	return s
}
