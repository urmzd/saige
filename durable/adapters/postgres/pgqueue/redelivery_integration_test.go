//go:build integration

package pgqueue_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

// Regression: Postgres judges a lease on the database clock, so a lease can
// lapse under a worker that is still executing the run (a clock step, a
// stalled heartbeat). The queue then delivers the run again and the same
// worker claims it. That redelivery must not run the body alongside itself
// and must not be charged to the run: the only failure is the one the queue
// counts for the lapse, and the run ends with its result.
func TestWorkerRedeliveryOfARunItIsExecutingBurnsNoBudget(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_queue_redeliver")
	q := newQueue(t, pool, m)

	lgr := ledger.NewMemory()
	reg := run.NewRegistry()
	var (
		inFlight   atomic.Int32
		prepared   atomic.Int32
		overlapped atomic.Bool
	)
	started := make(chan struct{}, 8)
	finish := make(chan struct{})
	wf := durable.ActivityIn(reg, "slow-turn", func(ctx context.Context, _ durable.Void) (string, error) {
		if inFlight.Add(1) > 1 {
			inFlight.Add(-1)
			overlapped.Store(true)
			return "", errors.New("run is already executing in this process")
		}
		defer inFlight.Add(-1)

		answer, err := durable.Step(ctx, "prepare", func(context.Context) (string, error) {
			prepared.Add(1)
			return "answer", nil
		})
		if err != nil {
			return "", err
		}
		started <- struct{}{}
		select {
		case <-finish:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return answer, nil
	})

	wctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	w := worker.New(lgr, q,
		worker.WithRegistry(reg),
		worker.WithLeaseTTL(30*time.Second), // only the test lapses the lease
		worker.WithBackoff(func(int) time.Duration { return time.Millisecond }),
		worker.WithJanitorEvery(0),
		worker.WithConcurrency(2),
	)
	go func() { _ = w.Run(wctx); close(done) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("worker did not stop")
		}
	})

	h, err := durable.Start(ctx, durable.New(lgr, q), wf, durable.Void{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitStarted := func(which string) {
		t.Helper()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			r, gerr := lgr.GetRun(ctx, h.RunID())
			t.Fatalf("the %s attempt never reached its wait: run is %s %q (%v)", which, r.Status, r.Error, gerr)
		}
	}
	awaitStarted("first")

	// The lease lapses on the database clock while the first attempt works.
	tag, err := pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET lease_until = now() - interval '1 second' WHERE run_id = $1 AND lease_until IS NOT NULL`,
		m.Table), h.RunID())
	if err != nil {
		t.Fatalf("lapse the lease: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("setup: lapsed %d leases, want 1", tag.RowsAffected())
	}

	// The redelivered attempt takes over and reaches the same wait by replay.
	awaitStarted("redelivered")
	close(finish)

	resCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := h.Result(resCtx)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out != "answer" {
		t.Fatalf("result = %q, want answer", out)
	}
	if overlapped.Load() {
		t.Fatal("the workflow body ran alongside itself")
	}
	if got := prepared.Load(); got != 1 {
		t.Fatalf("step executions = %d, want 1 (the second attempt replays it)", got)
	}

	var attempt, failures int
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT attempt, failures FROM %s WHERE run_id = $1`, m.Table), h.RunID()).Scan(&attempt, &failures); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if attempt != 2 || failures != 1 {
		t.Fatalf("attempt, failures = %d, %d; want 2, 1 (one redelivery, charged only for the lapse)", attempt, failures)
	}
}
