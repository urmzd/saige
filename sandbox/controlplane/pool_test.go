package controlplane_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

// gate is a tool that blocks every call until released, recording how many
// calls run at once and the order they started in.
type gate struct {
	release chan struct{}
	started chan string

	mu      sync.Mutex
	running int
	peak    int
	order   []string
	ctxErrs atomic.Int32 // calls that ended because their context did
}

func newGate() *gate {
	return &gate{release: make(chan struct{}), started: make(chan string, 64)}
}

func (g *gate) call(ctx context.Context, _ tool.Runtime, in []byte) ([]byte, error) {
	g.mu.Lock()
	g.running++
	g.peak = max(g.peak, g.running)
	g.order = append(g.order, string(in))
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.running--
		g.mu.Unlock()
	}()
	g.started <- string(in)
	select {
	case <-g.release:
		return in, nil
	case <-ctx.Done():
		g.ctxErrs.Add(1)
		return nil, ctx.Err()
	}
}

func (g *gate) waitStarted(t *testing.T, n int) []string {
	t.Helper()
	var got []string
	for range n {
		select {
		case s := <-g.started:
			got = append(got, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d calls started: %v", len(got), n, got)
		}
	}
	return got
}

func (g *gate) assertNoneStart(t *testing.T) {
	t.Helper()
	select {
	case s := <-g.started:
		t.Fatalf("call %q started with no node free", s)
	case <-time.After(50 * time.Millisecond):
	}
}

func gatedPlane(t *testing.T, opts ...controlplane.Option) (*controlplane.Memory, *gate, *metrics.Memory) {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := newGate()
	registry := tool.NewRegistry()
	if err := registry.Register(tool.Func("gate", g.call)); err != nil {
		t.Fatal(err)
	}
	rec := metrics.NewMemory()
	plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), rec, opts...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plane.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return plane, g, rec
}

func submit(t *testing.T, plane *controlplane.Memory, name string, inputs ...string) []string {
	t.Helper()
	ids := make([]string, len(inputs))
	for i, in := range inputs {
		id, err := plane.SubmitAsync(context.Background(), name, task.Task{Tool: "gate", Input: []byte(in)})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

func TestPoolBoundsConcurrencyAndRunsInOrder(t *testing.T) {
	ctx := context.Background()
	plane, g, rec := gatedPlane(t)
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc", Replicas: 2}); err != nil {
		t.Fatal(err)
	}

	ids := submit(t, plane, "svc", "a", "b", "c", "d", "e")
	g.waitStarted(t, 2)
	g.assertNoneStart(t)

	st, err := plane.Status(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	busy := 0
	for _, n := range st.Nodes {
		if n.Busy {
			busy++
		}
	}
	if st.Pending != 3 || busy != 2 {
		t.Fatalf("status = %+v, want 3 pending and 2 busy", st)
	}
	if got := rec.Snapshot()[`sandbox_tasks_pending{deployment="svc"}`]; got != 3 {
		t.Fatalf("pending gauge = %v, want 3", got)
	}

	close(g.release)
	for _, id := range ids {
		res := awaitResult(t, plane, "svc", id)
		if res.Error != "" {
			t.Fatalf("task %s: %s", id, res.Error)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", g.peak)
	}
	// The two first tasks start in either order; the backlog drains
	// oldest first.
	if got := g.order[2:]; got[0] != "c" || got[1] != "d" || got[2] != "e" {
		t.Fatalf("backlog ran in order %v, want c d e", g.order)
	}
}

func TestPoolBacklogIsBounded(t *testing.T) {
	ctx := context.Background()
	plane, g, _ := gatedPlane(t, controlplane.WithBacklog(1))
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	submit(t, plane, "svc", "running", "waiting")
	g.waitStarted(t, 1)
	_, err := plane.SubmitAsync(ctx, "svc", task.Task{Tool: "gate", Input: []byte("refused")})
	if !errors.Is(err, controlplane.ErrFull) {
		t.Fatalf("want ErrFull, got %v", err)
	}
	close(g.release)
}

func TestPoolScaleDownDrainsAndScaleUpTakesTheBacklog(t *testing.T) {
	ctx := context.Background()
	plane, g, _ := gatedPlane(t)
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	ids := submit(t, plane, "svc", "a", "b")
	g.waitStarted(t, 2)

	// Shrinking never cancels work: both busy nodes stay until their tasks
	// return.
	if err := plane.Scale(ctx, "svc", 0); err != nil {
		t.Fatal(err)
	}
	if st, _ := plane.Status(ctx, "svc"); st.Replicas != 0 || len(st.Nodes) != 2 {
		t.Fatalf("after scale to zero with busy nodes: %+v", st)
	}
	g.release <- struct{}{}
	g.release <- struct{}{}
	for _, id := range ids {
		if res := awaitResult(t, plane, "svc", id); res.Error != "" {
			t.Fatalf("a scale-down cancelled task %s: %s", id, res.Error)
		}
	}
	waitFor(t, func() bool {
		st, _ := plane.Status(ctx, "svc")
		return len(st.Nodes) == 0
	})

	// With no nodes a task waits; growing picks it up.
	later := submit(t, plane, "svc", "later")
	g.assertNoneStart(t)
	if err := plane.Scale(ctx, "svc", 1); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t, 1)
	close(g.release)
	awaitResult(t, plane, "svc", later[0])
}

func TestDeployWithNegativeReplicasWaitsForScale(t *testing.T) {
	ctx := context.Background()
	plane, g, _ := gatedPlane(t)
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc", Replicas: -1}); err != nil {
		t.Fatal(err)
	}
	ids := submit(t, plane, "svc", "x")
	g.assertNoneStart(t)
	if _, done, err := plane.Result(ctx, "svc", ids[0]); err != nil || done {
		t.Fatalf("waiting task: done=%v err=%v", done, err)
	}
	if err := plane.Scale(ctx, "svc", 1); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	awaitResult(t, plane, "svc", ids[0])
}

func TestTasksOutliveTheirSubmitter(t *testing.T) {
	plane, g, _ := gatedPlane(t)
	if err := plane.Deploy(context.Background(), controlplane.ServiceSpec{Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := plane.Submit(ctx, "svc", task.Task{ID: "t1", Tool: "gate", Input: []byte("x")})
		errc <- err
	}()
	g.waitStarted(t, 1)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit after cancel: %v", err)
	}
	close(g.release)
	if res := awaitResult(t, plane, "svc", "t1"); string(res.Output) != "x" {
		t.Fatalf("task did not finish on its own: %+v", res)
	}
	if g.ctxErrs.Load() != 0 {
		t.Fatal("the submitter's context reached the task")
	}
}

func TestResultsAreRetainedAndBounded(t *testing.T) {
	ctx := context.Background()
	plane, g, _ := gatedPlane(t, controlplane.WithResultRetention(2))
	close(g.release)
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, in := range []string{"1", "2", "3"} {
		res, err := plane.Submit(ctx, "svc", task.Task{Tool: "gate", Input: []byte(in)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.TaskID)
	}
	if _, _, err := plane.Result(ctx, "svc", ids[0]); !errors.Is(err, controlplane.ErrUnknownTask) {
		t.Fatalf("oldest result: want ErrUnknownTask, got %v", err)
	}
	for _, id := range ids[1:] {
		if res, done, err := plane.Result(ctx, "svc", id); err != nil || !done || res.TaskID != id {
			t.Fatalf("retained result %s: %+v %v %v", id, res, done, err)
		}
	}
	if _, _, err := plane.Result(ctx, "svc", "never-submitted"); !errors.Is(err, controlplane.ErrUnknownTask) {
		t.Fatalf("unknown task: want ErrUnknownTask, got %v", err)
	}
	if _, err := plane.SubmitAsync(ctx, "svc", task.Task{ID: ids[2], Tool: "gate"}); err == nil {
		t.Fatal("a task ID the deployment still knows was accepted twice")
	}
}

func TestCloseCancelsRunningAndFailsWaitingTasks(t *testing.T) {
	ctx := context.Background()
	plane, g, _ := gatedPlane(t)
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	ids := submit(t, plane, "svc", "running", "waiting")
	g.waitStarted(t, 1)

	if err := plane.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if g.ctxErrs.Load() != 1 {
		t.Fatal("Close returned before the running task saw its context end")
	}
	running, done, err := plane.Result(ctx, "svc", ids[0])
	if err != nil || !done || running.Error == "" {
		t.Fatalf("running task after close: %+v %v %v", running, done, err)
	}
	waiting, done, err := plane.Result(ctx, "svc", ids[1])
	if err != nil || !done || waiting.Error != controlplane.ErrClosed.Error() {
		t.Fatalf("waiting task after close: %+v %v %v", waiting, done, err)
	}
	if _, err := plane.SubmitAsync(ctx, "svc", task.Task{Tool: "gate"}); !errors.Is(err, controlplane.ErrClosed) {
		t.Fatalf("submit after close: %v", err)
	}
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "other"}); !errors.Is(err, controlplane.ErrClosed) {
		t.Fatalf("deploy after close: %v", err)
	}
	if st, _ := plane.Status(ctx, "svc"); len(st.Nodes) != 0 {
		t.Fatalf("nodes left after close: %+v", st)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
