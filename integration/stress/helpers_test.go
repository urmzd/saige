//go:build stress

package stress

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/postgres"
)

// envInt reads a positive integer from name, or returns def.
func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", name, v)
	}
	return n
}

// testContext returns a context that ends with the test or after d.
func testContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// percentile returns the p-th percentile (0 < p <= 100) of sorted ds by the
// nearest-rank method.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p/100*float64(len(sorted))+0.5) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// logLatency reports p50, p99, max and throughput for one batch of
// operations that took total wall time.
func logLatency(t *testing.T, what string, ds []time.Duration, total time.Duration) {
	t.Helper()
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	t.Logf("%s: n=%d p50=%v p99=%v max=%v wall=%v throughput=%.1f/s",
		what, len(sorted), percentile(sorted, 50), percentile(sorted, 99), sorted[len(sorted)-1],
		total.Round(time.Millisecond), float64(len(sorted))/total.Seconds())
}

// checkGoroutines fails the test when, after a settling period, more
// goroutines run than before plus slack. Call it with the count taken before
// the work started. Runtime and test-framework goroutines come and go, so a
// small slack avoids flakes without hiding a per-run leak.
func checkGoroutines(t *testing.T, before int) {
	t.Helper()
	const slack = 5
	deadline := time.Now().Add(10 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= before+slack {
			t.Logf("goroutines: before=%d after=%d", before, now)
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: before=%d after=%d\n%s", before, now, buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pgDatabase returns the URL of a fresh database on the server named by
// SAIGE_TEST_POSTGRES_DSN, dropped when the test ends. It skips the test when
// the variable is unset.
func pgDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL stress test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "saige_stress_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// pgPool returns a migrated SAIGE pool on a fresh database. maxConns sizes
// the pool for the test's concurrency.
func pgPool(t *testing.T, maxConns int) *pgxpool.Pool {
	t.Helper()
	dbURL := pgDatabase(t)
	ctx := context.Background()
	// postgres.NewPool registers pgvector types at connect time, so the
	// extension must exist first.
	boot, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		t.Fatalf("create vector extension: %v", err)
	}
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dbURL, MaxConns: int32(maxConns)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return pool
}
