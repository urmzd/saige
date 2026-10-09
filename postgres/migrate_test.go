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

func TestCheckServerVersion(t *testing.T) {
	tests := []struct {
		name    string
		num     int
		version string
		wantErr bool
	}{
		{name: "postgres 16", num: 160004, version: "16.4", wantErr: true},
		{name: "postgres 17", num: 170006, version: "17.6", wantErr: true},
		{name: "postgres 18.0", num: 180000, version: "18.0"},
		{name: "postgres 18 minor", num: 180006, version: "18.6"},
		{name: "postgres 19", num: 190000, version: "19.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkServerVersion(tt.num, tt.version)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkServerVersion(%d) = %v, wantErr %v", tt.num, err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrUnsupportedServer) {
				t.Errorf("error %v does not wrap ErrUnsupportedServer", err)
			}
			if !strings.Contains(err.Error(), "PostgreSQL 18") || !strings.Contains(err.Error(), tt.version) {
				t.Errorf("error %q should name the server version and the requirement", err)
			}
		})
	}
}

// TestRunMigrationsCreatesSearchExtensions checks that a fresh database gets
// both required extensions and the BM25 index over variant text.
func TestRunMigrationsCreatesSearchExtensions(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()
	if err := RunMigrations(ctx, pool, MigrationOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var num int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&num); err != nil {
		t.Fatal(err)
	}
	if num < MinServerVersionNum {
		t.Fatalf("migrations succeeded on server_version_num %d", num)
	}
	for _, ext := range []string{"vector", "pg_search"} {
		var installed bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, ext).Scan(&installed); err != nil {
			t.Fatal(err)
		}
		if !installed {
			t.Errorf("extension %s not created", ext)
		}
	}
	var hasIndex bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('idx_rag_variant_bm25') IS NOT NULL`).Scan(&hasIndex); err != nil {
		t.Fatal(err)
	}
	if !hasIndex {
		t.Error("idx_rag_variant_bm25 not created")
	}
}
