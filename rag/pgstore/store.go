// Package pgstore implements rag/types.Store using PostgreSQL + pgvector.
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	agenttypes "github.com/urmzd/saige/agent/types"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/rag/types"
)

var (
	_ types.Store                = (*Store)(nil)
	_ types.DocumentReplacer     = (*Store)(nil)
	_ types.VariantRecordGetter  = (*Store)(nil)
	_ types.VariantRecordsGetter = (*Store)(nil)
	_ types.DocumentLister       = (*Store)(nil)
	_ types.SourceFinder         = (*Store)(nil)
	_ types.SourceLister         = (*Store)(nil)
)

// Store implements types.Store backed by PostgreSQL with pgvector.
type Store struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	iterativeScan IterativeScan
	efSearch      int
	maxScanTuples int
	// optionErr holds the first invalid option; searches return it.
	optionErr error

	versionMu    sync.Mutex
	versionKnown bool
	iterativeOK  bool
}

// Config names the database a Store uses.
type Config struct {
	// Pool is the database, from postgres.NewPool. Required. The caller
	// owns it: Close does not close it.
	Pool *pgxpool.Pool
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// New creates a PostgreSQL-backed RAG store. Options tune filtered vector
// search; see WithIterativeScan, WithEFSearch, and WithMaxScanTuples. A nil
// pool or an invalid option is an error wrapping types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (*Store, error) {
	if cfg.Pool == nil {
		return nil, fmt.Errorf("%w: rag pgstore: Config.Pool is required", agenttypes.ErrInvalidConfig)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{pool: cfg.Pool, logger: logger}
	for _, opt := range opts {
		opt(s)
	}
	if s.optionErr != nil {
		return nil, fmt.Errorf("%w: %w", agenttypes.ErrInvalidConfig, s.optionErr)
	}
	return s, nil
}

// Close is a no-op; the pool is externally managed.
func (s *Store) Close(_ context.Context) error {
	return nil
}

// encodeMetadata marshals metadata to JSON for JSONB columns.
func encodeMetadata(meta map[string]string) []byte {
	if meta == nil {
		return nil
	}
	b, _ := json.Marshal(meta)
	return b
}

// decodeMetadata unmarshals JSONB bytes to metadata map.
func decodeMetadata(b []byte) map[string]string {
	if b == nil {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal(b, &m)
	return m
}
