// Package pgstore implements rag/types.Store using PostgreSQL + pgvector.
package pgstore

import (
	"context"
	"encoding/json"
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

// NewStore creates a new PostgreSQL-backed RAG store. Options tune filtered
// vector search; see WithIterativeScan, WithEFSearch, and WithMaxScanTuples.
// An invalid option makes every SearchByEmbedding call return an error.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger, opts ...Option) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{pool: pool, logger: logger}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
