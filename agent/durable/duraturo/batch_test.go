package duraturo

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	dt "github.com/urmzd/duraturo"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
)

// gatedModel answers once release is closed, standing in for a vendor
// batch that takes hours.
type gatedModel struct {
	release chan struct{}
	calls   atomic.Int32
}

func (g *gatedModel) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	msgs := req.Messages
	g.calls.Add(1)
	out := make(chan types.Delta, 4)
	go func() {
		defer close(out)
		select {
		case <-g.release:
		case <-ctx.Done():
			out <- types.ErrorDelta{Error: ctx.Err()}
			return
		}
		u := msgs[len(msgs)-1].(types.UserMessage).Parts[0].(types.TextPart).Text
		for _, d := range agenttest.TextResponse("done: " + u) {
			out <- d
		}
	}()
	return out, nil
}

// countingLocal counts vendor submits.
type countingLocal struct {
	*batch.Local
	submits atomic.Int32
}

func (c *countingLocal) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	c.submits.Add(1)
	return c.Local.Submit(ctx, reqs, opts)
}

// TestAwaitBatchParksAndResumes checks that a workflow awaiting a batch
// parks off the queue, is woken by the watcher when the batch ends, reads
// the results once, and never submits twice.
func TestAwaitBatchParksAndResumes(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	model := &gatedModel{release: make(chan struct{})}
	vendor := &countingLocal{Local: batch.NewLocal(model, 2)}
	jobs := batch.NewRunner(vendor, batch.NewMemoryStore(), batch.WithPollInterval(5*time.Millisecond, 5*time.Millisecond))
	reqs := []types.BatchRequest{
		{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Text("one"))}},
		{CustomID: "b", Messages: []types.Message{types.UserMsg(types.Text("two"))}},
	}
	var bodies atomic.Int32
	wf := dt.ActivityIn(e.registry, "batch.test", func(ctx context.Context, _ string) ([]string, error) {
		bodies.Add(1)
		results, err := e.AwaitBatch(ctx, jobs, "job-1", reqs)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(results))
		for i, r := range results {
			out[i] = r.CustomID + "=" + r.Text()
		}
		return out, nil
	})
	startWorker(t, e)
	h, err := dt.Start(ctx, e.client, wf, "go", dt.WithRunID("run-1"))
	if err != nil {
		t.Fatal(err)
	}

	// The run submits, records its owner and parks.
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := jobs.Get(ctx, "job-1")
		if err == nil && job.Owner == "run-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job was not submitted by the workflow")
		}
		time.Sleep(5 * time.Millisecond)
	}
	watchCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = e.WatchBatches(watchCtx, jobs, 5*time.Millisecond) }()
	time.Sleep(50 * time.Millisecond)
	close(model.release)

	out, err := h.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != "a=done: one" || out[1] != "b=done: two" {
		t.Fatalf("results = %v", out)
	}
	if n := vendor.submits.Load(); n != 1 {
		t.Fatalf("vendor submits = %d, want 1", n)
	}
	if bodies.Load() < 2 {
		t.Fatalf("workflow bodies = %d, want a park and a resume", bodies.Load())
	}
	if model.calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2", model.calls.Load())
	}
}
