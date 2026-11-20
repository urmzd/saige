package durable_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/adapters/file/fileledger"
	"github.com/urmzd/saige/durable/adapters/file/filequeue"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

// RunOne executes a named run in the calling process and returns with its
// outcome in the ledger; another RunOne of the same run in flight gets
// queue.ErrLeased.
func TestRunOneExecutesANamedRunHere(t *testing.T) {
	dir := t.TempDir()
	lgr, _ := fileledger.New(dir)
	q, _ := filequeue.New(dir)
	reg := run.NewRegistry()
	entered, release := make(chan struct{}), make(chan struct{})
	double := durable.ActivityIn(reg, "double", func(_ context.Context, n int) (int, error) {
		close(entered)
		<-release
		return 2 * n, nil
	})
	c := durable.New(lgr, q)
	ctx := context.Background()
	if _, err := durable.Start(ctx, c, double, 21, durable.WithRunID("r1")); err != nil {
		t.Fatal(err)
	}
	w := worker.New(lgr, q, worker.WithRegistry(reg))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.RunOne(ctx, "r1"); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	if err := w.RunOne(ctx, "r1"); !errors.Is(err, queue.ErrLeased) {
		t.Fatalf("second RunOne: %v, want ErrLeased", err)
	}
	close(release)
	wg.Wait()
	got, err := durable.HandleFor[int](c, "r1").Result(ctx)
	if err != nil || got != 42 {
		t.Fatalf("result %d, %v", got, err)
	}
	if err := w.RunOne(ctx, "r1"); !errors.Is(err, queue.ErrNotQueued) {
		t.Fatalf("RunOne of a finished run: %v, want ErrNotQueued", err)
	}
}

// Cancel ends a run that has not finished, and the run never executes.
func TestCancelEndsAPendingRun(t *testing.T) {
	dir := t.TempDir()
	lgr, _ := fileledger.New(dir)
	q, _ := filequeue.New(dir)
	reg := run.NewRegistry()
	var ran atomic.Int32
	noop := durable.ActivityIn(reg, "noop", func(context.Context, int) (int, error) { ran.Add(1); return 0, nil })
	c := durable.New(lgr, q)
	ctx := context.Background()
	if _, err := durable.Start(ctx, c, noop, 1, durable.WithRunID("r1")); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(ctx, "r1", "the person changed their mind"); err != nil {
		t.Fatal(err)
	}
	r, err := lgr.GetRun(ctx, "r1")
	if err != nil || !run.Cancelled(r) {
		t.Fatalf("run %+v, %v; want cancelled", r, err)
	}
	if err := worker.New(lgr, q, worker.WithRegistry(reg)).RunOne(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 {
		t.Fatal("a cancelled run executed")
	}
	if err := c.Cancel(ctx, "r1", ""); !errors.Is(err, run.ErrAlreadyTerminal) {
		t.Fatalf("second cancel: %v", err)
	}
}
