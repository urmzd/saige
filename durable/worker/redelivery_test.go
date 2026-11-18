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

	mu             sync.Mutex
	claims         []queue.Item
	failedReleases int
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
	if it.Attempt == 1 {
		return time.Time{}, errors.New("heartbeat: no connection available")
	}
	return q.Memory.Heartbeat(ctx, it, heldLease)
}

func (q *lapsingQueue) Release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) error {
	if failed {
		q.mu.Lock()
		q.failedReleases++
		q.mu.Unlock()
	}
	return q.Memory.Release(ctx, it, delay, failed)
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

	startWorker(t, testWorker(lgr, q, reg))
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
