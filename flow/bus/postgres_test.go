package bus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/bus/bustest"
)

// postgresEnv names the Postgres the conformance suite runs against. Without
// it the Postgres subtests skip.
const postgresEnv = "SAIGE_TEST_POSTGRES_DSN"

var schemas atomic.Uint64

// newPostgresPool returns a pool whose search_path is a schema of its own,
// dropped when the test ends, so buses built on it cannot see each other.
func newPostgresPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_bus_%d_%d", time.Now().UnixNano(), schemas.Add(1))
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse $%s: %v", postgresEnv, err)
	}
	cfg.MaxConns = 8
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
	return pool
}

func TestPostgresConformance(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s to run the Postgres bus conformance suite", postgresEnv)
	}
	bustest.Run(t, func(t *testing.T, cfg bustest.Config) bus.Bus {
		b := bus.NewPostgres(newPostgresPool(t, url),
			bus.WithRetention(cfg.Retention),
			bus.WithBacklog(cfg.Backlog),
			bus.WithPollInterval(25*time.Millisecond),
			bus.WithLease(cfg.Lease),
		)
		if err := b.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		return b
	})
}

// Two processes on one database are one bus: what one publishes, a reader
// built on the other's pool receives. That is the point of a shared backend.
func TestPostgresIsSharedAcrossBusesOnOneDatabase(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s", postgresEnv)
	}
	pool := newPostgresPool(t, url)
	a, b := bus.NewPostgres(pool), bus.NewPostgres(pool)
	if err := a.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(t.Context()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	s, err := b.Subscribe(t.Context(), all)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := a.Publish(t.Context(), runEvent("run_1", 1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e, err := s.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 1 || e.Run != "run_1" {
		t.Fatalf("got %s#%d, want run_1#1", e.Run, e.Seq)
	}
}

// Two replicas on one database are two members of one group: each event is
// handled by one of them, and when the one holding the lease dies the other
// takes over from its cursor.
func TestPostgresGroupSpansReplicas(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s", postgresEnv)
	}
	pool := newPostgresPool(t, url)
	opts := []bus.Option{bus.WithPollInterval(25 * time.Millisecond), bus.WithLease(400 * time.Millisecond)}
	one, two := bus.NewPostgres(pool, opts...), bus.NewPostgres(pool, opts...)
	if err := one.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 4; seq++ {
		if err := one.Publish(t.Context(), runEvent("run_1", seq)); err != nil {
			t.Fatal(err)
		}
	}

	dying, die := context.WithCancel(t.Context())
	a, err := one.Consume(dying, "egress", all)
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first, err := a.Recv(wait)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Commit(wait, first.Pos); err != nil {
		t.Fatal(err)
	}

	b, err := two.Consume(t.Context(), "egress", all)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	probe, stop := context.WithTimeout(t.Context(), 300*time.Millisecond)
	if d, err := b.Recv(probe); err == nil {
		t.Fatalf("the second replica received %s while the first held the lease", d.Event.ID)
	}
	stop()

	die()
	var seqs []uint64
	for range 3 {
		d, err := b.Recv(wait)
		if err != nil {
			t.Fatalf("Recv after takeover: %v", err)
		}
		seqs = append(seqs, d.Event.Seq)
	}
	if want := []uint64{2, 3, 4}; !slices.Equal(seqs, want) {
		t.Fatalf("after takeover got %v, want %v", seqs, want)
	}
}

// A publish waits only on its own tenant: the lock is per tenant, not per
// schema.
func TestPostgresPublishLockIsPerTenant(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s", postgresEnv)
	}
	pool := newPostgresPool(t, url)
	b := bus.NewPostgres(pool)
	if err := b.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('fiber_events:' || current_schema() || ':acme', 0))`); err != nil {
		t.Fatal(err)
	}

	other := runEvent("run_9", 1)
	other.Tenant = "globex"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := b.Publish(ctx, other); err != nil {
		t.Fatalf("a publish to globex waited on acme's lock: %v", err)
	}
	blocked, release := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer release()
	if err := b.Publish(blocked, runEvent("run_1", 1)); err == nil {
		t.Fatal("a publish to acme did not wait for acme's lock")
	}
}

// A table created before positions existed is numbered per tenant in the
// order its rows were appended, and publishing continues from there.
func TestPostgresMigrateNumbersRowsWrittenBeforePositions(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s", postgresEnv)
	}
	pool := newPostgresPool(t, url)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `CREATE TABLE fiber_events (
	id bigserial PRIMARY KEY, tenant text NOT NULL, channel text NOT NULL, type text NOT NULL,
	run text NOT NULL DEFAULT '', seq bigint NOT NULL DEFAULT 0, event_id text NOT NULL,
	body text NOT NULL, at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		tenant string
		seq    uint64
	}{{"acme", 1}, {"globex", 1}, {"acme", 2}} {
		e := runEvent("run_1", row.seq)
		e.Tenant = row.tenant
		e.Time = time.Now().UTC()
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO fiber_events (tenant, channel, type, run, seq, event_id, body) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.Tenant, e.Channel, e.Type, e.Run, int64(e.Seq), e.ID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	b := bus.NewPostgres(pool)
	if err := b.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := b.Publish(ctx, runEvent("run_1", 3)); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT tseq, seq FROM fiber_events WHERE tenant = 'acme' ORDER BY tseq`)
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]int64
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	rows.Close()
	if want := [][2]int64{{1, 1}, {2, 2}, {3, 3}}; !slices.Equal(got, want) {
		t.Fatalf("acme's (position, seq) = %v, want %v", got, want)
	}
}
