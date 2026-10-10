package batch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func fastRunner(p types.BatchProvider, store Store, opts ...RunnerOption) *Runner {
	return must.Get(NewRunner(RunnerConfig{Provider: p, Store: store}, append([]RunnerOption{WithPollInterval(time.Millisecond, 5*time.Millisecond)}, opts...)...))
}

// TestRunnerMapsOutOfOrderResults checks that results the vendor returns in
// any order come back under the caller's IDs, in request order from Run.
func TestRunnerMapsOutOfOrderResults(t *testing.T) {
	v := newFakeVendor()
	r := fastRunner(v, NewMemoryStore())
	reqs := requests("case a/sample 1", "case b", "c#2")
	got, err := r.Run(context.Background(), "job-1", reqs)
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range got {
		if res.CustomID != reqs[i].CustomID {
			t.Fatalf("result %d id = %q, want %q", i, res.CustomID, reqs[i].CustomID)
		}
		if want := "answer to q-" + reqs[i].CustomID; res.Text() != want {
			t.Fatalf("result %d = %q, want %q", i, res.Text(), want)
		}
	}
	// The vendor saw safe wire IDs, not the caller's.
	for _, b := range v.batches {
		for _, q := range b.reqs {
			if !batchIDSafe(q.CustomID) {
				t.Fatalf("wire id %q is not vendor safe", q.CustomID)
			}
		}
	}
	job, _ := r.Get(context.Background(), "job-1")
	if job.State != JobCollected {
		t.Fatalf("state = %s, want collected", job.State)
	}
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

func batchIDSafe(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	return !strings.ContainsFunc(id, func(c rune) bool { return !strings.ContainsRune(idAlphabet, c) })
}

// TestRunnerResumeAfterRestart checks that a second process, with a new
// runner over the same file store, resumes the batch the first submitted
// instead of submitting it again.
func TestRunnerResumeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	v := newFakeVendor()
	v.autoEnd = false
	ctx := context.Background()
	reqs := requests("a", "b")

	store1, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := fastRunner(v, store1).Submit(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	// The first process exits here. The batch ends while nobody watches.
	v.end(job.Handle.ID, types.BatchEnded)

	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	r2 := fastRunner(v, store2)
	got, err := r2.Run(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if v.submitCount() != 1 {
		t.Fatalf("vendor submits = %d, want 1", v.submitCount())
	}
	if len(got) != 2 || got[0].Text() != "answer to q-a" {
		t.Fatalf("results = %+v", got)
	}
	// Different requests under the same job ID are refused.
	if _, err := r2.Submit(ctx, "job", requests("a", "c")); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("err = %v, want ErrManifestMismatch", err)
	}
}

// TestRunnerCrashMidSubmitFinds checks that a submit interrupted after the
// vendor created the batch, but before its ID was saved, is resolved by a
// vendor lookup on the next Submit, without a duplicate batch.
func TestRunnerCrashMidSubmitFinds(t *testing.T) {
	v := newFakeVendor()
	v.failNext, v.created = errTransport, true
	r := fastRunner(findingVendor{v}, NewMemoryStore())
	ctx := context.Background()
	reqs := requests("a", "b", "c")

	if _, err := r.Submit(ctx, "job", reqs); !errors.Is(err, ErrSubmitIncomplete) {
		t.Fatalf("err = %v, want ErrSubmitIncomplete", err)
	}
	job, err := r.Submit(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobSubmitted || job.Handle == nil || job.Handle.ID != "fb_1" {
		t.Fatalf("job = %+v, want the first batch", job)
	}
	if v.submitCount() != 1 {
		t.Fatalf("vendor submits = %d, want 1", v.submitCount())
	}
}

// TestRunnerCrashMidSubmitIndeterminate checks D-14: without a lookup the
// job stops as indeterminate, and Reconcile with the handle resumes it.
func TestRunnerCrashMidSubmitIndeterminate(t *testing.T) {
	v := newFakeVendor()
	v.failNext, v.created = errTransport, true
	r := fastRunner(v, NewMemoryStore())
	ctx := context.Background()
	reqs := requests("a")

	if _, err := r.Submit(ctx, "job", reqs); !errors.Is(err, ErrSubmitIncomplete) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.Submit(ctx, "job", reqs); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("err = %v, want ErrIndeterminate", err)
	}
	if _, err := r.Wait(ctx, "job"); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("wait err = %v, want ErrIndeterminate", err)
	}
	if _, err := r.Reconcile(ctx, "job", Resolution{Handle: &types.BatchHandle{Provider: "fake", ID: "fb_1"}}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Run(ctx, "job", reqs)
	if err != nil || len(got) != 1 || got[0].Outcome != types.BatchSucceeded {
		t.Fatalf("run = %+v, %v", got, err)
	}
	if v.submitCount() != 1 {
		t.Fatalf("vendor submits = %d, want 1", v.submitCount())
	}

	// Resubmit is the other resolution.
	v.failNext, v.created = errTransport, true
	_, _ = r.Submit(ctx, "job2", reqs)
	_, _ = r.Submit(ctx, "job2", reqs)
	if _, err := r.Reconcile(ctx, "job2", Resolution{Resubmit: true}); err != nil {
		t.Fatal(err)
	}
	if job, err := r.Submit(ctx, "job2", reqs); err != nil || job.State != JobSubmitted {
		t.Fatalf("resubmit = %+v, %v", job, err)
	}
}

// TestRunnerDefiniteSubmitFailure checks that a rejected submit fails the
// job and releases the budget it reserved.
func TestRunnerDefiniteSubmitFailure(t *testing.T) {
	v := newFakeVendor()
	v.failNext = &types.ProviderError{Provider: "fake", Kind: types.ErrorKindInvalidRequest, Err: errors.New("bad")}
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), PerCallCost: types.USD(0.1)})
	r := fastRunner(v, NewMemoryStore(), WithBudget(b))
	if _, err := r.Submit(context.Background(), "job", requests("a", "b")); err == nil {
		t.Fatal("submit succeeded")
	}
	job, _ := r.Get(context.Background(), "job")
	if job.State != JobFailed {
		t.Fatalf("state = %s, want failed", job.State)
	}
	if b.Remaining() != types.USD(1) {
		t.Fatalf("remaining = %s, want the whole limit back", b.Remaining())
	}
}

// TestRunnerBudgetSettlement checks reservation at submit, settlement at the
// batch rate card, and that failed and expired requests are not charged.
func TestRunnerBudgetSettlement(t *testing.T) {
	v := newFakeVendor()
	v.autoEnd = false
	v.outcome = func(r types.BatchRequest) types.BatchOutcome {
		switch lastText(r.Messages) {
		case "q-b":
			return types.BatchErrored
		case "q-c":
			return types.BatchExpiredOutcome
		}
		return types.BatchSucceeded
	}
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), PerCallCost: types.USD(0.1)})
	r := fastRunner(v, NewMemoryStore(), WithBudget(b))
	ctx := context.Background()
	reqs := requests("a", "b", "c")
	job, err := r.Submit(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Remaining(); got != types.USD(0.7) {
		t.Fatalf("remaining after submit = %s, want 0.7 (three requests reserved)", got)
	}
	v.end(job.Handle.ID, types.BatchExpired)
	if _, err := r.Wait(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	got, err := r.Collect(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != types.BatchSucceeded || got[1].Outcome != types.BatchErrored || got[2].Outcome != types.BatchExpiredOutcome {
		t.Fatalf("outcomes = %s %s %s", got[0].Outcome, got[1].Outcome, got[2].Outcome)
	}
	// One billed request: 1000 input and 100 output tokens at half of
	// 2 and 10 per million is 0.0010 + 0.0005.
	if want := types.USD(0.0015); b.Spent() != want {
		t.Fatalf("spent = %s, want %s", b.Spent(), want)
	}
	if b.Remaining() != types.USD(1)-types.USD(0.0015) {
		t.Fatalf("remaining = %s; reservations were not all released", b.Remaining())
	}
	if b.Usage().Requests != 1 {
		t.Fatalf("billed requests = %d, want 1", b.Usage().Requests)
	}
}

// TestRunnerMissingResultIsUncertain checks that a request the vendor left
// out of its results is reported errored and charged its reservation.
func TestRunnerMissingResultIsUncertain(t *testing.T) {
	v := newFakeVendor()
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), PerCallCost: types.USD(0.1)})
	r := fastRunner(v, NewMemoryStore(), WithBudget(b))
	ctx := context.Background()
	reqs := requests("a", "b")
	job, err := r.Submit(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	v.drop = map[string]bool{wireID(job.Prefix, 1): true}
	got, err := r.Run(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Outcome != types.BatchErrored || got[1].Err == nil {
		t.Fatalf("missing result = %+v", got[1])
	}
	if b.Uncertain() != 1 {
		t.Fatalf("uncertain = %d, want 1", b.Uncertain())
	}
}

// TestRunnerNotifierWakesWait checks that Wait returns on a notification
// published by another runner's Sweep, well before its poll interval.
func TestRunnerNotifierWakesWait(t *testing.T) {
	v := newFakeVendor()
	v.autoEnd = false
	n := notify.NewMemory(16)
	store := NewMemoryStore()
	waiter := must.Get(NewRunner(RunnerConfig{Provider: v, Store: store}, WithNotifier(n, ""), WithPollInterval(time.Hour, time.Hour)))
	sweeper := must.Get(NewRunner(RunnerConfig{Provider: v, Store: store}, WithNotifier(n, "")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, err := waiter.Submit(ctx, "job", requests("a"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Job, 1)
	go func() {
		j, _ := waiter.Wait(ctx, "job")
		done <- j
	}()
	time.Sleep(50 * time.Millisecond)
	v.end(job.Handle.ID, types.BatchEnded)
	ended, err := sweeper.Sweep(ctx)
	if err != nil || len(ended) != 1 {
		t.Fatalf("sweep = %v, %v", ended, err)
	}
	select {
	case j := <-done:
		if j.State != JobEnded {
			t.Fatalf("state = %s", j.State)
		}
	case <-ctx.Done():
		t.Fatal("wait did not wake")
	}
}

// TestRunnerEphemeralResubmit checks that a local batch lost with its
// process is submitted again.
func TestRunnerEphemeralResubmit(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	reqs := requests("a")
	p := &echoProvider{}
	if _, err := fastRunner(NewLocal(p, 2), store).Submit(ctx, "job", reqs); err != nil {
		t.Fatal(err)
	}
	// A new process has a new Local that never saw the batch.
	got, err := fastRunner(NewLocal(p, 2), store).Run(ctx, "job", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text() != "echo: q-a" {
		t.Fatalf("result = %+v", got[0])
	}
}

func TestRunnerCancel(t *testing.T) {
	v := newFakeVendor()
	v.autoEnd = false
	r := fastRunner(v, NewMemoryStore())
	ctx := context.Background()
	if _, err := r.Submit(ctx, "job", requests("a")); err != nil {
		t.Fatal(err)
	}
	job, err := r.Cancel(ctx, "job")
	if err != nil || job.State != JobEnded || job.Status.State != types.BatchCanceled {
		t.Fatalf("cancel = %+v, %v", job, err)
	}
}
