package local

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/types"
)

func TestAwaitWithoutNotifier(t *testing.T) {
	e := New(t.TempDir())
	if _, err := e.Await(context.Background(), "run", Resumable); !errors.Is(err, ErrNoNotifier) {
		t.Fatalf("Await = %v, want ErrNoNotifier", err)
	}
}

// A reply recorded through another engine value, as another process would,
// wakes a waiter without polling.
func TestAwaitWakesOnReply(t *testing.T) {
	ctx := context.Background()
	n := notify.NewMemory(0)
	defer n.Close()
	dir := t.TempDir()
	waiter := &Engine{Directory: dir, ApprovalTTL: time.Hour, Notifier: n}
	host := &Engine{Directory: dir, ApprovalTTL: time.Hour, Notifier: n}
	suspendedRun(t, waiter, "run")
	in := question("run")
	if _, err := host.Router().Post(ctx, in); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("Post = %v", err)
	}

	type result struct {
		s   State
		err error
	}
	got := make(chan result, 1)
	go func() {
		s, err := waiter.Await(ctx, "run", func(s State) bool { return s.Interrupts[in.ID].Decision != nil })
		got <- result{s, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("Await returned before the reply: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if err := host.Router().Reply(ctx, types.InterruptReply{ID: in.ID, IdempotencyKey: "k", Decision: types.ApprovalDecision{Approved: true}}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !Resumable(r.s) {
			t.Fatal("decided run is not resumable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await not woken by Reply")
	}
}

func TestAwaitWakesOnAppendAndCancel(t *testing.T) {
	ctx := context.Background()
	n := notify.NewMemory(0)
	defer n.Close()
	e := &Engine{Directory: t.TempDir(), ApprovalTTL: time.Hour, Notifier: n}
	suspendedRun(t, e, "run")
	s, _ := e.Inspect("run")
	if Resumable(s) {
		t.Fatal("suspended run without a reply is resumable")
	}

	done := make(chan error, 1)
	go func() { _, err := e.Await(ctx, "run", Resumable); done <- err }()
	time.Sleep(20 * time.Millisecond)
	if err := e.Append("run", "v1", "msg-1", []types.Message{types.UserMsg(types.Text("more"))}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await not woken by Append")
	}

	go func() {
		_, err := e.Await(ctx, "run", func(s State) bool { return s.Status == statusCancelled })
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := e.Cancel("run", "v1"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await not woken by Cancel")
	}
}

func TestAwaitHonorsContext(t *testing.T) {
	n := notify.NewMemory(0)
	defer n.Close()
	e := &Engine{Directory: t.TempDir(), Notifier: n}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.Await(ctx, "missing", Resumable); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await = %v", err)
	}
}

type failingNotifier struct{ types.Notifier }

func (failingNotifier) Publish(context.Context, string, []byte) error {
	return errors.New("down")
}

func TestSignalFailureKeepsChange(t *testing.T) {
	e := &Engine{Directory: t.TempDir(), ApprovalTTL: time.Hour, Notifier: failingNotifier{}}
	suspendedRun(t, e, "run")
	err := e.Append("run", "v1", "k", []types.Message{types.UserMsg(types.Text("x"))})
	if !errors.Is(err, ErrSignal) {
		t.Fatalf("Append = %v, want ErrSignal", err)
	}
	s, _ := e.Inspect("run")
	if len(s.Inputs) != 1 {
		t.Fatal("append not saved")
	}
}
