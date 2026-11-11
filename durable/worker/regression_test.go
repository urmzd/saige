package worker_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

// Regression: review finding — parked runs must never burn retry budget.
// With a tight MaxAttempts and an aggressive janitor re-enqueueing the
// parked run every tick, the run must survive many park/resume cycles and
// still complete once signaled.
func TestRegression_ParkBurnsNoBudget(t *testing.T) {
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()
	c := durable.New(lgr, q)

	wf := durable.ActivityIn(reg, "gated", func(ctx context.Context, _ durable.Void) (string, error) {
		return durable.Event[string](ctx, "approval")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := worker.New(lgr, q,
		worker.WithRegistry(reg),
		worker.WithLeaseTTL(30*time.Millisecond),
		worker.WithHeartbeatEvery(10*time.Millisecond),
		worker.WithJanitorEvery(15*time.Millisecond), // hammer the parked run
		worker.WithConcurrency(2),
	)
	go func() { _ = w.Run(ctx) }()

	h, err := durable.Start(context.Background(), c, wf, durable.Void{}, durable.WithMaxAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	// Let the janitor drive many park/resume cycles — far more than the
	// budget of 2 — while the event stays absent.
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		r, gerr := lgr.GetRun(context.Background(), h.RunID())
		if gerr != nil {
			t.Fatal(gerr)
		}
		if r.Status.Terminal() {
			t.Fatalf("run went terminal while merely waiting: %s %q", r.Status, r.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := c.Signal(context.Background(), h.RunID(), "approval", "granted"); err != nil {
		t.Fatal(err)
	}
	resCtx, resCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resCancel()
	out, err := h.Result(resCtx)
	if err != nil {
		t.Fatalf("run failed after signal: %v", err)
	}
	if out != "granted" {
		t.Fatalf("result = %q, want granted", out)
	}
}

// ctxCheckQueue fails the test if any outcome verb arrives on an
// already-canceled context — the review-confirmed graceful-shutdown bug.
type ctxCheckQueue struct {
	*queue.Memory
	t        *testing.T
	released atomic.Int32
}

func (c *ctxCheckQueue) Release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) error {
	if ctx.Err() != nil {
		c.t.Errorf("Release issued on canceled context")
	}
	if failed {
		c.t.Errorf("shutdown release must not consume retry budget")
	}
	c.released.Add(1)
	return c.Memory.Release(ctx, it, delay, failed)
}

func (c *ctxCheckQueue) Settle(ctx context.Context, it queue.Item) error {
	if ctx.Err() != nil {
		c.t.Errorf("Settle issued on canceled context")
	}
	return c.Memory.Settle(ctx, it)
}

// Regression: review finding — graceful shutdown must persist outcomes on a
// cancellation-detached context and hand interrupted runs back without
// burning budget; a second worker then finishes the run.
func TestRegression_GracefulShutdownReleasesCleanly(t *testing.T) {
	lgr := ledger.NewMemory()
	q := &ctxCheckQueue{Memory: queue.NewMemory(), t: t}
	reg := run.NewRegistry()
	c := durable.New(lgr, q)

	started := make(chan struct{}, 4)
	block := make(chan struct{})
	var executions atomic.Int32
	wf := durable.ActivityIn(reg, "slow", func(ctx context.Context, _ durable.Void) (string, error) {
		executions.Add(1)
		started <- struct{}{}
		select {
		case <-block:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})

	ctx1, cancel1 := context.WithCancel(context.Background())
	w1 := worker.New(lgr, q,
		worker.WithRegistry(reg),
		worker.WithLeaseTTL(30*time.Second), // expiry must NOT be the recovery path
		worker.WithJanitorEvery(0),
		worker.WithConcurrency(1),
	)
	w1Done := make(chan struct{})
	go func() { _ = w1.Run(ctx1); close(w1Done) }()

	h, err := durable.Start(context.Background(), c, wf, durable.Void{}, durable.WithMaxAttempts(1))
	if err != nil {
		t.Fatal(err)
	}
	<-started // worker 1 is mid-run
	cancel1() // graceful shutdown: the run must be RELEASED, not abandoned
	<-w1Done

	if q.released.Load() == 0 {
		t.Fatal("shutdown did not release the in-flight run")
	}

	// MaxAttempts is 1: if the shutdown had consumed budget, worker 2 would
	// terminally fail the run instead of completing it.
	close(block)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	w2 := worker.New(lgr, q,
		worker.WithRegistry(reg),
		worker.WithLeaseTTL(30*time.Second),
		worker.WithJanitorEvery(0),
		worker.WithConcurrency(1),
	)
	go func() { _ = w2.Run(ctx2) }()

	resCtx, resCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resCancel()
	out, err := h.Result(resCtx)
	if err != nil {
		t.Fatalf("run failed after clean shutdown handoff: %v", err)
	}
	if out != "done" {
		t.Fatalf("result = %q, want done", out)
	}
	if got := executions.Load(); got != 2 {
		t.Fatalf("executions = %d, want 2 (interrupted once, completed once)", got)
	}
}

// lateClaimQueue delays the answer to its first claim until the caller has
// stopped: the claim takes its lease at once, but Claim returns it only
// once ctx is done, as a backend waiting out a claim it already sent does.
// It records how that lease is handed back.
type lateClaimQueue struct {
	*queue.Memory
	t        *testing.T
	once     sync.Once
	claimed  chan struct{} // closed once the late claim holds its lease
	released atomic.Int32
}

func (q *lateClaimQueue) Claim(ctx context.Context, ttl time.Duration) (queue.Item, time.Time, error) {
	it, deadline, err := q.Memory.Claim(ctx, ttl)
	if err != nil {
		return it, deadline, err
	}
	late := false
	q.once.Do(func() { late = true })
	if late {
		close(q.claimed)
		<-ctx.Done()
	}
	return it, deadline, nil
}

func (q *lateClaimQueue) Release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) error {
	if ctx.Err() != nil {
		q.t.Errorf("Release issued on canceled context")
	}
	if failed || delay != 0 {
		q.t.Errorf("Release(delay=%v, failed=%v), want an immediate release that burns no budget", delay, failed)
	}
	q.released.Add(1)
	return q.Memory.Release(ctx, it, delay, failed)
}

// Regression: a claim whose answer lands after the worker has stopped holds
// a lease the stopping worker owns. The worker must hand it straight back,
// unexecuted and burning no budget, so the run is claimable at once rather
// than leased to nobody until the TTL lapses.
func TestRegression_ClaimLandingAtStopIsHandedBack(t *testing.T) {
	lgr := ledger.NewMemory()
	q := &lateClaimQueue{Memory: queue.NewMemory(), t: t, claimed: make(chan struct{})}
	reg := run.NewRegistry()
	c := durable.New(lgr, q)

	var executions atomic.Int32
	wf := durable.ActivityIn(reg, "late-claimed", func(context.Context, durable.Void) (string, error) {
		executions.Add(1)
		return "done", nil
	})
	h, err := durable.Start(context.Background(), c, wf, durable.Void{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := worker.New(lgr, q,
		worker.WithRegistry(reg),
		worker.WithLeaseTTL(30*time.Second), // expiry must NOT be the recovery path
		worker.WithJanitorEvery(0),
		worker.WithConcurrency(1),
	)
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	select {
	case <-q.claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never claimed the run")
	}
	cancel() // stop while the claim's answer is still on its way
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}

	if got := executions.Load(); got != 0 {
		t.Fatalf("executions = %d, want 0: a stopping worker must not run what it claims", got)
	}
	if got := q.released.Load(); got != 1 {
		t.Fatalf("releases = %d, want 1: the late claim must be handed back", got)
	}
	claimCtx, claimCancel := context.WithTimeout(context.Background(), time.Second)
	defer claimCancel()
	it, _, err := q.Memory.Claim(claimCtx, time.Minute)
	if err != nil {
		t.Fatalf("run not claimable after the stop (leased to nobody?): %v", err)
	}
	if it.RunID != h.RunID() || it.Attempt != 2 || it.Failures != 0 {
		t.Fatalf("reclaim = %+v, want {%s 2 0}", it, h.RunID())
	}
}
