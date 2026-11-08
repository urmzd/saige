package worker_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

// workerHandle owns one running worker's lifecycle.
type workerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startWorker runs w in the background until stop (or test cleanup); stop
// waits for Run to return, so after it the worker holds no claims and no
// goroutines.
func startWorker(t *testing.T, w *worker.Worker) *workerHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &workerHandle{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		_ = w.Run(ctx)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *workerHandle) stop() {
	h.cancel()
	<-h.done
}

// testWorker builds a worker with millisecond-scale timing and the janitor
// disabled; extra options override the base set.
func testWorker(lgr ledger.Ledger, q queue.Queue, reg *run.Registry, extra ...worker.Option) *worker.Worker {
	opts := []worker.Option{
		worker.WithRegistry(reg),
		worker.WithConcurrency(2),
		worker.WithLeaseTTL(50 * time.Millisecond),
		worker.WithHeartbeatEvery(15 * time.Millisecond),
		worker.WithBackoff(func(int) time.Duration { return time.Millisecond }),
		worker.WithJanitorEvery(0),
	}
	opts = append(opts, extra...)
	return worker.New(lgr, q, opts...)
}

// resultCtx bounds a Handle.Result wait.
func resultCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// awaitParked polls until runID shows the park signature: pending in the
// ledger and nothing claimable in the queue. Claims the poll steals from the
// worker are released immediately. The empty-queue check must hold twice,
// one lease apart, so a run merely leased mid-execution is never mistaken
// for a parked one.
func awaitParked(t *testing.T, lgr ledger.Ledger, q queue.Queue, runID string, leaseTTL time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	confirmed := false
	for time.Now().Before(deadline) {
		r, _, err := lgr.Load(ctx, runID)
		if err != nil {
			t.Fatalf("load %s: %v", runID, err)
		}
		if r.Status.Terminal() {
			t.Fatalf("run %s reached %s while awaiting park: %s", runID, r.Status, r.Error)
		}
		claimCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		it, _, err := q.Claim(claimCtx, 10*time.Millisecond)
		cancel()
		if err == nil {
			// Not parked: hand the claim back and keep waiting.
			_ = q.Release(ctx, it, 0, false)
			confirmed = false
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if confirmed {
			return
		}
		confirmed = true
		time.Sleep(leaseTTL + 10*time.Millisecond)
	}
	t.Fatalf("run %s never parked", runID)
}

// waitForRecords polls the ledger until every key has a record.
func waitForRecords(t *testing.T, lgr ledger.Ledger, runID string, keys ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, records, err := lgr.Load(context.Background(), runID)
		if err == nil {
			have := make(map[string]bool, len(records))
			for _, rec := range records {
				have[rec.Key] = true
			}
			missing := false
			for _, k := range keys {
				if !have[k] {
					missing = true
					break
				}
			}
			if !missing {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("records %v never appeared for run %s", keys, runID)
}

func TestWorker_HappyPath(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	var reserveN, chargeN, notifyN atomic.Int64
	reserve := durable.ActivityIn(reg, "reserve", func(_ context.Context, n int) (int, error) {
		reserveN.Add(1)
		return n + 10, nil
	})
	charge := durable.ActivityIn(reg, "charge", func(_ context.Context, n int) (int, error) {
		chargeN.Add(1)
		return n * 2, nil
	})
	notify := durable.ActivityIn(reg, "notify", func(_ context.Context, n int) (int, error) {
		notifyN.Add(1)
		return n - 3, nil
	})
	wf := durable.ActivityIn(reg, "order", func(ctx context.Context, n int) (int, error) {
		a, err := reserve.Call(ctx, n)
		if err != nil {
			return 0, err
		}
		b, err := charge.Call(ctx, a)
		if err != nil {
			return 0, err
		}
		return notify.Call(ctx, b)
	})

	startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, 5)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, err := h.Result(resultCtx(t))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if want := (5+10)*2 - 3; out != want {
		t.Fatalf("output = %d, want %d", out, want)
	}
	if reserveN.Load() != 1 || chargeN.Load() != 1 || notifyN.Load() != 1 {
		t.Fatalf("invocations = %d/%d/%d, want 1/1/1",
			reserveN.Load(), chargeN.Load(), notifyN.Load())
	}
}

func TestWorker_UnknownWorkflow(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()

	clientReg := run.NewRegistry()
	wf := durable.ActivityIn(clientReg, "ghost", func(_ context.Context, s string) (string, error) {
		return s, nil
	})

	// The worker's registry has never heard of "ghost".
	startWorker(t, testWorker(lgr, q, run.NewRegistry()))

	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, "in", durable.WithMaxAttempts(2))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = h.Result(resultCtx(t))
	var rfe *durable.RunFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("result error = %v, want *RunFailedError", err)
	}
	if !strings.Contains(rfe.Message, "unknown workflow") || !strings.Contains(rfe.Message, "ghost") {
		t.Fatalf("failure message = %q, want unknown-workflow mention", rfe.Message)
	}
}

func TestWorker_RetryableBurnsAttempts(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	var execs atomic.Int64
	wf := durable.ActivityIn(reg, "flaky", func(_ context.Context, s string) (string, error) {
		execs.Add(1)
		return "", errors.New("boom")
	})

	startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, "x", durable.WithMaxAttempts(3))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = h.Result(resultCtx(t))
	var rfe *durable.RunFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("result error = %v, want *RunFailedError", err)
	}
	if !strings.Contains(rfe.Message, run.ErrMaxAttempts.Error()) || !strings.Contains(rfe.Message, "boom") {
		t.Fatalf("failure message = %q, want max-attempts wrapping boom", rfe.Message)
	}
	if got := execs.Load(); got != 3 {
		t.Fatalf("executions = %d, want 3 (one per attempt)", got)
	}
}

func TestWorker_NonRetryableFailsFirstAttempt(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	var validateN, wfN atomic.Int64
	validate := durable.ActivityIn(reg, "validate", func(_ context.Context, s string) (string, error) {
		validateN.Add(1)
		return "", durable.NonRetryable(errors.New("bad input"))
	})
	wf := durable.ActivityIn(reg, "intake", func(ctx context.Context, s string) (string, error) {
		wfN.Add(1)
		return validate.Call(ctx, s)
	})

	startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, "x", durable.WithMaxAttempts(5))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = h.Result(resultCtx(t))
	var rfe *durable.RunFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("result error = %v, want *RunFailedError", err)
	}
	if !strings.Contains(rfe.Message, "bad input") {
		t.Fatalf("failure message = %q, want the non-retryable cause", rfe.Message)
	}
	if validateN.Load() != 1 || wfN.Load() != 1 {
		t.Fatalf("executions = %d/%d, want 1/1: non-retryable must not burn attempts",
			validateN.Load(), wfN.Load())
	}

	// The failure itself is memoized.
	_, records, err := lgr.Load(context.Background(), h.RunID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, rec := range records {
		if rec.Name == "validate" && rec.Status == run.RecordFailed {
			found = true
			if !strings.Contains(rec.Error, "bad input") {
				t.Fatalf("failed record error = %q, want the cause", rec.Error)
			}
		}
	}
	if !found {
		t.Fatalf("no failed record for %q; records = %v", "validate", records)
	}
}

func TestWorker_ParkAndSignalResume(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	wf := durable.ActivityIn(reg, "approval-flow", func(ctx context.Context, in string) (string, error) {
		v, err := durable.Event[string](ctx, "approval")
		if err != nil {
			return "", err
		}
		return in + ":" + v, nil
	})

	startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, "req", durable.WithMaxAttempts(1000))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// The run must sit pending, off the queue, burning nothing.
	awaitParked(t, lgr, q, h.RunID(), 50*time.Millisecond)

	if err := c.Signal(context.Background(), h.RunID(), "approval", "yes"); err != nil {
		t.Fatalf("signal: %v", err)
	}
	out, err := h.Result(resultCtx(t))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if out != "req:yes" {
		t.Fatalf("output = %q, want %q", out, "req:yes")
	}
}

func TestWorker_ParkJanitorBackstop(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	wf := durable.ActivityIn(reg, "approval-flow", func(ctx context.Context, in string) (string, error) {
		v, err := durable.Event[string](ctx, "approval")
		if err != nil {
			return "", err
		}
		return in + ":" + v, nil
	})

	// Worker 1 (no janitor) parks the run; stopping it leaves the run
	// pending in the ledger with nothing in the queue.
	w1 := startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, "req", durable.WithMaxAttempts(1000))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitParked(t, lgr, q, h.RunID(), 50*time.Millisecond)
	w1.stop()

	// A human writes the event record straight into the ledger — no
	// Signal, no Enqueue. Only the janitor can wake the run now.
	if _, err := lgr.Record(context.Background(), run.Record{
		RunID: h.RunID(), Key: "event:approval#0", Kind: run.KindEvent,
		Name: "approval", Status: run.RecordOK, Output: []byte(`"yes"`),
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}

	startWorker(t, testWorker(lgr, q, reg, worker.WithJanitorEvery(25*time.Millisecond)))
	out, err := h.Result(resultCtx(t))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if out != "req:yes" {
		t.Fatalf("output = %q, want %q", out, "req:yes")
	}
}
