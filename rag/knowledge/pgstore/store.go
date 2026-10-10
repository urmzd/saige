// Package pgstore implements knowledge/types.Store using PostgreSQL + pgvector.
package pgstore

import (
	"fmt"
	agenttypes "github.com/urmzd/saige/agent/types"
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

// Option configures a Store.
type Option func(*Store)

// WithTraversalLimits caps the neighbors and edges one GetNode call
// returns. A traversal that reaches either cap stops and reports
// NodeDetail.Truncated. Values of zero or less keep the defaults
// (DefaultMaxNodes, DefaultMaxEdges).
func WithTraversalLimits(maxNodes, maxEdges int) Option {
	return func(s *Store) {
		if maxNodes > 0 {
			s.maxNodes = maxNodes
		}
		if maxEdges > 0 {
			s.maxEdges = maxEdges
		}
	}
}

// Config names the database a Store uses.
type Config struct {
	// Pool is the database, already connected; schema migration is
	// separate (postgres.RunMigrations). Required. The caller owns it.
	Pool *pgxpool.Pool
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// New creates a PostgreSQL-backed knowledge store. A nil pool is an error
// wrapping types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (*Store, error) {
	if cfg.Pool == nil {
		return nil, fmt.Errorf("%w: knowledge pgstore: Config.Pool is required", agenttypes.ErrInvalidConfig)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{pool: cfg.Pool, logger: logger, maxNodes: DefaultMaxNodes, maxEdges: DefaultMaxEdges}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}
