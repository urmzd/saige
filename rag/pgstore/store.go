// Package pgstore implements rag/types.Store using PostgreSQL + pgvector.
package pgstore

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/rag/types"
)

var (
	_ types.Store            = (*Store)(nil)
	_ types.DocumentReplacer = (*Store)(nil)
)

// Store implements types.Store backed by PostgreSQL with pgvector.
type Store struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewStore creates a new PostgreSQL-backed RAG store.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{pool: pool, logger: logger}
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
