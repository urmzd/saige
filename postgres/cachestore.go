package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/types"
)

// CacheStoreOptions configures a CacheStore. The zero value is valid.
type CacheStoreOptions struct {
	// DefaultTTL applies when Set receives a zero TTL. Zero stores such
	// entries without expiry.
	DefaultTTL time.Duration
}

// CacheStore is a types.Cache[[]byte] in the UNLOGGED saige_cache table. It
// is the shared byte store behind the response cache and the tool cache:
//
//	store := postgres.NewCacheStore(pool, postgres.CacheStoreOptions{})
//	responses := cache.BytesCache(store)     // agent/provider/cache
//	toolResults := toolcache.BytesCache(store) // agent/toolcache
//
// An UNLOGGED table skips the write-ahead log, so writes are cheap and the
// table is emptied after a crash, which a cache tolerates. Expiry uses the
// database clock, so every process agrees on it. Get ignores expired rows;
// Sweep or StartSweeper deletes them. For a per-process level in front of
// it, kept coherent through a Notifier, wrap it in notify.Cache.
// RunMigrations creates the table.
type CacheStore struct {
	pool *pgxpool.Pool
	opts CacheStoreOptions
}

var _ types.Cache[[]byte] = (*CacheStore)(nil)

// NewCacheStore returns a cache store on pool.
func NewCacheStore(pool *pgxpool.Pool, opts CacheStoreOptions) *CacheStore {
	return &CacheStore{pool: pool, opts: opts}
}

// Get implements types.Cache. An expired entry is a miss.
func (s *CacheStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var value []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM saige_cache
		 WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`,
		key,
	).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache store: get: %w", err)
	}
	return value, true, nil
}

// Set implements types.Cache. A zero ttl uses DefaultTTL; a negative ttl
// stores an entry that is already expired.
func (s *CacheStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl == 0 {
		ttl = s.opts.DefaultTTL
	}
	var micros *int64
	if ttl != 0 {
		m := ttl.Microseconds()
		micros = &m
	}
	if value == nil {
		value = []byte{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO saige_cache (key, value, expires_at)
		 VALUES ($1, $2, now() + $3::bigint * interval '1 microsecond')
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, micros,
	)
	if err != nil {
		return fmt.Errorf("cache store: set: %w", err)
	}
	return nil
}

// Delete implements types.Cache.
func (s *CacheStore) Delete(ctx context.Context, key string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM saige_cache WHERE key = $1`, key); err != nil {
		return fmt.Errorf("cache store: delete: %w", err)
	}
	return nil
}

// Sweep deletes expired entries and returns how many it removed.
func (s *CacheStore) Sweep(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM saige_cache WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("cache store: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

// StartSweeper runs Sweep every interval until stop is called or ctx ends.
// A failed sweep is logged with logger (slog.Default() when nil) and retried
// at the next tick. stop waits for a running sweep to finish.
func (s *CacheStore) StartSweeper(ctx context.Context, interval time.Duration, logger *slog.Logger) (stop func()) {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
					logger.Warn("cache store: sweep failed", "err", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
