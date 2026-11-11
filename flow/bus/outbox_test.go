package bus_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/bus"
)

// newOutboxBus returns a migrated Postgres bus with a state table of the
// caller's own beside it, on a schema of the test's own.
func newOutboxBus(t *testing.T) (*bus.Postgres, *pgxpool.Pool, func(string) int) {
	t.Helper()
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s", postgresEnv)
	}
	pool := newPostgresPool(t, url)
	b := bus.NewPostgres(pool, bus.WithPollInterval(25*time.Millisecond))
	if err := b.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE TABLE runs (id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	count := func(query string) int {
		var n int
		if err := pool.QueryRow(t.Context(), query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return b, pool, count
}

// A state change and the event announcing it commit together: once the
// transaction commits, a consumer group reads the event.
func TestPostgresPublishTxCommitsWithTheStateChange(t *testing.T) {
	b, pool, count := newOutboxBus(t)
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO runs VALUES ('run_1')`); err != nil {
			return err
		}
		return b.PublishTx(t.Context(), tx, runEvent("run_1", 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM runs`); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	g, err := b.Consume(t.Context(), "observer", all)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	wait, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d, err := g.Recv(wait)
	if err != nil {
		t.Fatal(err)
	}
	if d.Event.Run != "run_1" || d.Event.Seq != 1 || d.Pos != 1 {
		t.Fatalf("got %s seq %d at %d, want run_1 seq 1 at 1", d.Event.Run, d.Event.Seq, d.Pos)
	}
}

// A transaction that rolls back leaves neither the state nor the event, and
// takes no position: the next event is the tenant's first.
func TestPostgresPublishTxRollsBackWithTheStateChange(t *testing.T) {
	b, pool, count := newOutboxBus(t)
	boom := errors.New("the state change failed after the event was written")
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO runs VALUES ('run_1')`); err != nil {
			return err
		}
		if err := b.PublishTx(t.Context(), tx, runEvent("run_1", 1)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n := count(`SELECT count(*) FROM runs`) + count(`SELECT count(*) FROM fiber_events`); n != 0 {
		t.Fatalf("a rolled back transaction left %d rows", n)
	}
	// The run's sequence was not reached either: publishing it again is
	// not a no-op.
	if err := b.Publish(t.Context(), runEvent("run_1", 1)); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM fiber_events WHERE tseq = 1 AND seq = 1`); n != 1 {
		t.Fatalf("after the rollback the event was not the tenant's first: %d rows at position 1", n)
	}
}

// PublishTx keeps Publish's dedupe: a sequence the run has reached is a
// no-op inside a transaction too, so a retried transaction writes it once.
func TestPostgresPublishTxIsIdempotent(t *testing.T) {
	b, pool, count := newOutboxBus(t)
	if err := b.Publish(t.Context(), runEvent("run_1", 1)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error { return b.PublishTx(t.Context(), tx, runEvent("run_1", 1)) })
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := count(`SELECT count(*) FROM fiber_events`); n != 1 {
		t.Fatalf("fiber_events = %d, want 1", n)
	}
}

// The tenant's lock is held until the caller's transaction ends, so a
// publish to the same tenant waits for it and takes the next position: no
// reader scanning by position steps over a row that commits late.
func TestPostgresPublishTxHoldsTheTenantLockUntilCommit(t *testing.T) {
	b, pool, count := newOutboxBus(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := b.PublishTx(t.Context(), tx, runEvent("run_1", 1)); err != nil {
		t.Fatal(err)
	}
	blocked, release := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer release()
	if err := b.Publish(blocked, runEvent("run_2", 1)); err == nil {
		t.Fatal("a publish to the same tenant did not wait for the open transaction")
	}
	done := make(chan error, 1)
	go func() { done <- b.Publish(t.Context(), runEvent("run_2", 1)) }()
	time.Sleep(100 * time.Millisecond)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM fiber_events WHERE (run, tseq) IN (('run_1', 1), ('run_2', 2))`); n != 2 {
		t.Fatalf("positions did not follow commit order: %d of 2 rows where expected", n)
	}
}

func TestPostgresPublishTxRefusesNoTransaction(t *testing.T) {
	b := bus.NewPostgres(nil)
	if err := b.PublishTx(t.Context(), nil, runEvent("run_1", 1)); !errors.Is(err, bus.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
