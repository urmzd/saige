package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// freshDatabase creates an empty database on the server named by
// SAIGE_TEST_POSTGRES_DSN and returns a pool connected to it. The database is
// dropped on cleanup. Tests are skipped when the variable is unset.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "saige_migrate_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	return pool
}

func TestRunMigrationsConcurrentStartup(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	const replicas = 4
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = RunMigrations(ctx, pool, MigrationOptions{})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
	}
}

func TestRunMigrationsEmbeddingDimension(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()
	if err := RunMigrations(ctx, pool, MigrationOptions{RAGEmbeddingDim: 768, KGEmbeddingDim: 768}); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	for _, tc := range []struct {
		name string
		opts MigrationOptions
		want error
	}{
		{"matching dimensions", MigrationOptions{RAGEmbeddingDim: 768, KGEmbeddingDim: 768}, nil},
		{"unset dimensions skip the check", MigrationOptions{}, nil},
		{"rag dimension changed", MigrationOptions{RAGEmbeddingDim: 1024}, ErrEmbeddingDimMismatch},
		{"kg dimension changed", MigrationOptions{KGEmbeddingDim: 1536}, ErrEmbeddingDimMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RunMigrations(ctx, pool, tc.opts)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("RunMigrations = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRunMigrationsStopsAtFirstFailure breaks an early statement and checks
// that the run returns its error without executing later statements.
func TestRunMigrationsStopsAtFirstFailure(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()
	// A view named like a table makes CREATE TABLE IF NOT EXISTS succeed but
	// the following ALTER TABLE fail.
	if _, err := pool.Exec(ctx, `CREATE VIEW kg_entity AS SELECT 1 AS id`); err != nil {
		t.Fatal(err)
	}
	err := RunMigrations(ctx, pool, MigrationOptions{})
	if err == nil {
		t.Fatal("RunMigrations succeeded on a broken schema")
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('agent_node') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("later statements ran after the first failure")
	}
}
