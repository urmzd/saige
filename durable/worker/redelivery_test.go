package worker_test

import (
	"context"
	"errors"
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

// lapsingQueue refuses every heartbeat of a run's first attempt with a
// transient error, the way an exhausted connection pool does, so that
// attempt's lease lapses while its worker is still executing it. Later
// attempts hold a lease far longer than the test, so exactly one lapse
// happens however slowly the test is scheduled. It records what the worker
// did about it.
const heldLease = time.Minute

type lapsingQueue struct {
	*queue.Memory

	// healed lets the first attempt's heartbeats through again, so it hears
	// the queue's own verdict.
	healed atomic.Bool

	mu             sync.Mutex
	claims         []queue.Item
	ended          map[queue.Item]int // Release and Settle calls per claimed item
	handedBack     []queue.Item       // claims the queue took back uncharged
	failedReleases int                // releases the queue charged to the run
}

// end records that the worker finished with a claim, whatever the queue
// answered: a wrapper that tracks what a process holds sees only the call.
func (q *lapsingQueue) end(it queue.Item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ended == nil {
		q.ended = make(map[queue.Item]int)
	}
	q.ended[it]++
}

// assertBalanced checks that every claim ended with exactly one Release or
// Settle call. Call it once the worker has stopped.
func (q *lapsingQueue) assertBalanced(t *testing.T) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.claims {
		if n := q.ended[it]; n != 1 {
			t.Errorf("claim %+v ended with %d Release or Settle calls, want exactly 1", it, n)
		}
	}
}

// awaitEnded waits, bounded, for the worker to finish with a claim.
func (q *lapsingQueue) awaitEnded(t *testing.T, it queue.Item) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		n := q.ended[it]
		q.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("claim %+v never ended with a Release or Settle call", it)
}

func (q *lapsingQueue) Claim(ctx context.Context, ttl time.Duration) (queue.Item, time.Time, error) {
	it, deadline, err := q.Memory.Claim(ctx, ttl)
	if err == nil && it.Attempt > 1 {
		deadline, err = q.Memory.Heartbeat(ctx, it, heldLease)
	}
	if err == nil {
		q.mu.Lock()
		q.claims = append(q.claims, it)
		q.mu.Unlock()
	}
	return it, deadline, err
}

func (q *lapsingQueue) Heartbeat(ctx context.Context, it queue.Item, ttl time.Duration) (time.Time, error) {
	if it.Attempt == 1 && !q.healed.Load() {
		return time.Time{}, errors.New("heartbeat: no connection available")
	}
	return q.Memory.Heartbeat(ctx, it, heldLease)
}

func (q *lapsingQueue) Release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) error {
	q.end(it)
	err := q.Memory.Release(ctx, it, delay, failed)
	if err == nil {
		q.mu.Lock()
		if failed {
			q.failedReleases++
		} else {
			q.handedBack = append(q.handedBack, it)
		}
		q.mu.Unlock()
	}
	return err
}

func (q *lapsingQueue) Settle(ctx context.Context, it queue.Item) error {
	q.end(it)
	return q.Memory.Settle(ctx, it)
}

// Regression: a run whose lease lapses while its worker is still executing
// it is delivered again, and the same worker claims it. Running it a second
// time alongside the first is refused by anything that allows one execution
// of a run per process, and each refusal used to be charged as a failure: the
// run spent its whole budget in a few backoffs and was completed as failed
// while its first attempt was still working. The redelivery must cost
// nothing beyond the one failure the queue charges for the lapse, the body
// must never run alongside itself, and the run must end with its result.
func TestWorkerRedeliveryOfARunItIsExecutingBurnsNoBudget(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := &lapsingQueue{Memory: queue.NewMemory()}
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

	w := startWorker(t, testWorker(lgr, q, reg))
	h, err := durable.Start(context.Background(), durable.New(lgr, q), wf, durable.Void{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// The first attempt works on while its lease lapses; the redelivered one
	// must take over and reach the same point by replay.
	for _, which := range []string{"first", "redelivered"} {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			r, gerr := lgr.GetRun(context.Background(), h.RunID())
			t.Fatalf("the %s attempt never reached its wait: run is %s %q (%v)", which, r.Status, r.Error, gerr)
		}
	}
	close(finish)

	out, err := h.Result(resultCtx(t))
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

	// Same-worker supersession: both claims end with one queue verb each.
	w.stop()
	q.assertBalanced(t)

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failedReleases != 0 {
		t.Fatalf("failed releases = %d, want 0: a redelivery is not a failure of the run", q.failedReleases)
	}
	if len(q.claims) != 2 {
		t.Fatalf("claims = %+v, want 2 (the first, and the redelivery after the lapse)", q.claims)
	}
	if last := q.claims[len(q.claims)-1]; last.Failures != 1 {
		t.Fatalf("failures at the last claim = %d, want 1 (the lapse itself)", last.Failures)
	}
}

// stalledFirstLoad is a ledger whose first Load blocks until its context is
// cancelled, the way a read on a stalled connection does.
type stalledFirstLoad struct {
	*ledger.Memory
	loads atomic.Int32
}

func (l *stalledFirstLoad) Load(ctx context.Context, runID string) (run.Run, []run.Record, error) {
	if l.loads.Add(1) == 1 {
		<-ctx.Done()
		return run.Run{}, nil, ctx.Err()
	}
	return l.Memory.Load(ctx, runID)
}

// Regression: the attempt a redelivery supersedes may not have reached the
// body yet. Its ledger read ran on the worker's context, so cancelling the
// attempt did not end it, and the successor waited for it forever while
// renewing its own lease. Everything an attempt blocks on must end when the
// attempt is cancelled.
func TestWorkerRedeliverySupersedesAnAttemptStalledBeforeItsBody(t *testing.T) {
	t.Parallel()
	lgr := &stalledFirstLoad{Memory: ledger.NewMemory()}
	q := &lapsingQueue{Memory: queue.NewMemory()}
	reg := run.NewRegistry()
	wf := durable.ActivityIn(reg, "greet", func(context.Context, durable.Void) (string, error) {
		return "hello", nil
	})

	w := startWorker(t, testWorker(lgr, q, reg))
	h, err := durable.Start(context.Background(), durable.New(lgr, q), wf, durable.Void{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := h.Result(ctx)
	w.stop()
	q.mu.Lock()
	claims, failed := append([]queue.Item(nil), q.claims...), q.failedReleases
	q.mu.Unlock()
	if err != nil || out != "hello" {
		t.Fatalf("result = %q, %v; want hello (loads=%d claims=%+v)", out, err, lgr.loads.Load(), claims)
	}
	if failed != 0 {
		t.Fatalf("failed releases = %d, want 0", failed)
	}
	if len(claims) != 2 || claims[1].Failures != 1 {
		t.Fatalf("claims = %+v, want 2, the second with the lapse's one failure", claims)
	}
	q.assertBalanced(t)
}

// A body that ignores cancellation cannot be made to stop. The redelivered
// claim must not sit in a claim slot for as long as that body runs: after a
// bounded wait it is handed back uncharged, the slot serves other runs, and
// the run resumes on the first delivery after the stuck attempt has gone.
func TestWorkerRedeliveryDoesNotWedgeASlotBehindABodyThatIgnoresCancellation(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := &lapsingQueue{Memory: queue.NewMemory()}
	reg := run.NewRegistry()

	var (
		runs       atomic.Int32
		inFlight   atomic.Int32
		overlapped atomic.Bool
	)
	stuck := make(chan struct{})
	unstick := sync.OnceFunc(func() { close(stuck) })
	stubborn := durable.ActivityIn(reg, "stubborn", func(ctx context.Context, _ durable.Void) (string, error) {
		if inFlight.Add(1) > 1 {
			overlapped.Store(true)
		}
		defer inFlight.Add(-1)
		if runs.Add(1) == 1 {
			<-stuck // deaf to ctx
			return "", ctx.Err()
		}
		return "answer", nil
	})
	other := durable.ActivityIn(reg, "other", func(context.Context, durable.Void) (string, error) {
		return "served", nil
	})

	// Two slots: the stuck first attempt keeps one for as long as it runs.
	w := startWorker(t, testWorker(lgr, q, reg))
	t.Cleanup(unstick) // runs before the worker's stop, which waits for the body
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, stubborn, durable.Void{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// The redelivery is handed back, uncharged, rather than held.
	deadline := time.Now().Add(5 * time.Second)
	for {
		q.mu.Lock()
		back := len(q.handedBack)
		q.mu.Unlock()
		if back > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the redelivered claim was never handed back while the first attempt ignored its cancellation")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The other slot is free: a different run is served while the body is stuck.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	served, err := durable.Exec(ctx, c, other, durable.Void{})
	if err != nil || served != "served" {
		t.Fatalf("a second run behind the stuck one = %q, %v; want served", served, err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("body executions while the first is stuck = %d, want 1", got)
	}

	// Once the stuck attempt returns, the next delivery runs the body.
	unstick()
	out, err := h.Result(ctx)
	if err != nil || out != "answer" {
		t.Fatalf("result = %q, %v; want answer", out, err)
	}
	if overlapped.Load() {
		t.Fatal("the workflow body ran alongside itself")
	}
	w.stop()
	q.assertBalanced(t)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failedReleases != 0 {
		t.Fatalf("failed releases = %d, want 0", q.failedReleases)
	}
	if last := q.claims[len(q.claims)-1]; last.Failures != 1 {
		t.Fatalf("failures at the last claim = %d, want 1 (the lapse itself)", last.Failures)
	}
}

// An attempt superseded by the queue's verdict (its heartbeat is answered
// run.ErrSuperseded because another worker holds the run now) still ends its
// claim with one Release call. The queue refuses it and charges nothing, but
// a wrapper that tracks what this process holds sees the claim end.
func TestWorkerSupersededAttemptStillEndsItsClaim(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := &lapsingQueue{Memory: queue.NewMemory()}
	reg := run.NewRegistry()

	started := make(chan struct{}, 1)
	wf := durable.ActivityIn(reg, "slow-turn", func(ctx context.Context, _ durable.Void) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})

	// One slot, so the redelivery goes to the other worker (the test).
	w := startWorker(t, testWorker(lgr, q, reg, worker.WithConcurrency(1)))
	h, err := durable.Start(context.Background(), durable.New(lgr, q), wf, durable.Void{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first attempt never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	successor, _, err := q.Memory.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatalf("claim the lapsed run as another worker: %v", err)
	}
	if successor.RunID != h.RunID() || successor.Attempt != 2 || successor.Failures != 1 {
		t.Fatalf("successor claim = %+v, want attempt 2 with the lapse's one failure", successor)
	}
	q.healed.Store(true) // the first attempt's next heartbeat hears the verdict

	first := queue.Item{RunID: h.RunID(), Attempt: 1}
	q.awaitEnded(t, first)
	w.stop()
	q.assertBalanced(t)

	q.mu.Lock()
	failed, back := q.failedReleases, len(q.handedBack)
	q.mu.Unlock()
	if failed != 0 || back != 0 {
		t.Fatalf("the queue accepted a release from the superseded attempt (charged=%d, uncharged=%d)", failed, back)
	}
	// The successor's claim is untouched: it still settles under its attempt.
	if err := q.Memory.Settle(ctx, successor); err != nil {
		t.Fatalf("successor settle after the superseded release: %v", err)
	}
	if r, err := lgr.GetRun(ctx, h.RunID()); err != nil || r.Status.Terminal() {
		t.Fatalf("run = %s, %v; a superseded attempt must not end the run", r.Status, err)
	}
}
