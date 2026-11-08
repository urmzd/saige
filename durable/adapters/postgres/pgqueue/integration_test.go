//go:build integration

package pgqueue_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/queue/queuetest"
)

// The integration harness owns all DDL: it applies pgqueue.RecommendedDDL
// through its own connection, because the adapter itself never creates or
// migrates anything.

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if url == "" {
		url = "postgres://postgres:test@localhost:5433/postgres?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New(%q): %v", url, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping %q: %v (is docker compose up?)", url, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var tableSeq atomic.Int64

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixMilli(), tableSeq.Add(1))
}

func applyDDL(t *testing.T, pool *pgxpool.Pool, ddl string) {
	t.Helper()
	for _, stmt := range strings.Split(ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("apply DDL %q: %v", stmt, err)
		}
	}
}

func dropTables(t *testing.T, pool *pgxpool.Pool, tables ...string) {
	t.Cleanup(func() {
		for _, tab := range tables {
			if _, err := pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tab); err != nil {
				t.Errorf("drop %s: %v", tab, err)
			}
		}
	})
}

func newQueue(t *testing.T, pool *pgxpool.Pool, m pgqueue.Mapping) queue.Queue {
	t.Helper()
	applyDDL(t, pool, pgqueue.RecommendedDDL(m))
	dropTables(t, pool, m.Table)

	q, err := pgqueue.New(pool, m, pgqueue.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return q
}

func TestQueueConformance(t *testing.T) {
	pool := testPool(t)
	queuetest.Run(t, func(t *testing.T) queue.Queue {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_queue")
		return newQueue(t, pool, m)
	})
}

func TestQueueConformanceScoped(t *testing.T) {
	pool := testPool(t)
	queuetest.Run(t, func(t *testing.T) queue.Queue {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_queue_scoped")
		m.Scope = pgqueue.Scope{Column: "source", Value: "anchor"}
		q := newQueue(t, pool, m)

		// Another application's immediately-claimable row shares the
		// table: any scope leak makes the suite claim it and fail.
		if _, err := pool.Exec(context.Background(), fmt.Sprintf(
			`INSERT INTO %s (run_id, ready_at, lease_until, attempt, source)
			 VALUES ('alien-run', now(), NULL, 0, 'other-app')`, m.Table)); err != nil {
			t.Fatalf("seed alien row: %v", err)
		}
		return q
	})
}

// TestQueueScopeLeavesAlienRowsAlone drives a scoped queue directly and then
// checks the other application's row byte for byte.
func TestQueueScopeLeavesAlienRowsAlone(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_queue_iso")
	m.Scope = pgqueue.Scope{Column: "source", Value: "anchor"}
	q := newQueue(t, pool, m)

	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (run_id, ready_at, lease_until, attempt, source)
		 VALUES ('alien-run', now(), NULL, 7, 'other-app')`, m.Table)); err != nil {
		t.Fatalf("seed alien row: %v", err)
	}

	// The alien row is claimable in its own scope but must be invisible
	// here.
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if it, _, err := q.Claim(shortCtx, time.Minute); err == nil {
		t.Fatalf("Claim leaked across scope: got %+v", it)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Claim error = %v, want context.DeadlineExceeded", err)
	}

	if err := q.Enqueue(ctx, "mine", 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	it, _, err := q.Claim(claimCtx, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if it.RunID != "mine" || it.Attempt != 1 {
		t.Fatalf("Claim = %+v, want {mine 1}", it)
	}
	if err := q.Settle(ctx, it); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	var readyNull bool
	var attempt int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT ready_at IS NULL, attempt FROM %s WHERE run_id = 'alien-run'", m.Table)).Scan(&readyNull, &attempt); err != nil {
		t.Fatalf("read alien row: %v", err)
	}
	if readyNull || attempt != 7 {
		t.Fatalf("alien row was touched: ready_at NULL=%v attempt=%d, want false/7", readyNull, attempt)
	}
}

// TestQueueValidateActionableErrors checks Validate's failure modes: the
// error says exactly what to create.
func TestQueueValidateActionableErrors(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("missing unique index", func(t *testing.T) {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_queue_noidx")
		applyDDL(t, pool, fmt.Sprintf(
			"CREATE TABLE %s (run_id text, ready_at timestamptz, lease_until timestamptz, attempt bigint, failures int)", m.Table))
		dropTables(t, pool, m.Table)

		q, err := pgqueue.New(pool, m)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = q.Validate(ctx)
		if err == nil {
			t.Fatal("Validate = nil, want error for missing unique index")
		}
		if !strings.Contains(err.Error(), "CREATE UNIQUE INDEX") {
			t.Fatalf("Validate error must suggest CREATE UNIQUE INDEX, got:\n%v", err)
		}
	})

	t.Run("non-plain unique index rejected", func(t *testing.T) {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_queue_nonplain")
		// A partial unique index and a deferrable unique constraint both
		// cover run_id, but Enqueue's ON CONFLICT arbiter inference
		// accepts neither: a laxer Validate would pass here and Enqueue
		// would then fail at runtime.
		applyDDL(t, pool, fmt.Sprintf(`
CREATE TABLE %[1]s (run_id text, ready_at timestamptz, lease_until timestamptz, attempt bigint, failures int);
CREATE UNIQUE INDEX ON %[1]s (run_id) WHERE ready_at IS NOT NULL;
ALTER TABLE %[1]s ADD CONSTRAINT %[1]s_uq UNIQUE (run_id) DEFERRABLE INITIALLY DEFERRED`, m.Table))
		dropTables(t, pool, m.Table)

		q, err := pgqueue.New(pool, m)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = q.Validate(ctx)
		if err == nil {
			t.Fatal("Validate = nil, want error: partial and deferrable unique indexes cannot back ON CONFLICT")
		}
		if !strings.Contains(err.Error(), "non-partial") || !strings.Contains(err.Error(), "CREATE UNIQUE INDEX") {
			t.Fatalf("Validate error must demand a plain unique index and suggest creating one, got:\n%v", err)
		}
	})

	t.Run("missing table", func(t *testing.T) {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_queue_missing")
		q, err := pgqueue.New(pool, m)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = q.Validate(ctx)
		if err == nil {
			t.Fatal("Validate = nil, want error for missing table")
		}
		if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "CREATE TABLE") {
			t.Fatalf("Validate error must say the table is missing and suggest CREATE TABLE, got:\n%v", err)
		}
	})

	t.Run("wrong column types", func(t *testing.T) {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_queue_badtypes")
		applyDDL(t, pool, fmt.Sprintf(`
CREATE TABLE %[1]s (run_id text, ready_at text, lease_until timestamptz, attempt text, failures int);
CREATE UNIQUE INDEX ON %[1]s (run_id)`, m.Table))
		dropTables(t, pool, m.Table)

		q, err := pgqueue.New(pool, m)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = q.Validate(ctx)
		if err == nil {
			t.Fatal("Validate = nil, want error for wrong column types")
		}
		if !strings.Contains(err.Error(), "timestamptz") || !strings.Contains(err.Error(), "bigint") {
			t.Fatalf("Validate error must name the wanted types, got:\n%v", err)
		}
	})
}
