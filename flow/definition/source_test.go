package definition_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/definition/definitiontest"
)

// postgresEnv names the Postgres the suite also runs against.
const postgresEnv = "SAIGE_TEST_POSTGRES_DSN"

var schemas atomic.Uint64

func TestMemorySource(t *testing.T) {
	definitiontest.Run(t, func(*testing.T) definition.Source { return definition.NewMemory() })
}

func TestPostgresSource(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("$%s is not set", postgresEnv)
	}
	definitiontest.Run(t, func(t *testing.T) definition.Source { return newPostgres(t, url) })
}

// A clock with nanoseconds, as Linux has, still reads back what it wrote:
// Postgres keeps microseconds, so a stamp finer than that would make an
// identical put look like a rewrite.
func TestPostgresSourceWithANanosecondClock(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("$%s is not set", postgresEnv)
	}
	ns := func() time.Time { return time.Date(2026, 10, 28, 21, 53, 5, 184170509, time.UTC) }
	definitiontest.Run(t, func(t *testing.T) definition.Source {
		return newPostgres(t, url, definition.WithClock(ns))
	})
}

// newPostgres returns a migrated source in a schema of its own.
func newPostgres(t *testing.T, url string, opts ...definition.Option) *definition.Postgres {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_%d_%d", time.Now().UnixNano(), schemas.Add(1))
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse $%s: %v", postgresEnv, err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		pool.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
		pool.Close()
	})
	src := definition.NewPostgres(pool, opts...)
	for range 2 {
		if err := src.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return src
}

// TestPostgresHasNoUpdatePath proves the store never rewrites a version: the
// only statements that touch fiber_definitions are SELECT and INSERT.
func TestPostgresHasNoUpdatePath(t *testing.T) {
	raw, err := os.ReadFile("postgres.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"UPDATE fiber_definitions", "DELETE FROM fiber_definitions", "UPDATE fiber_definition_retirements", "DELETE FROM fiber_definition_retirements", "DO UPDATE"} {
		if contains(string(raw), verb) {
			t.Errorf("postgres.go contains %q", verb)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
