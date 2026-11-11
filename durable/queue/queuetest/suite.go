// Package queuetest is the conformance suite every queue backend must pass.
// Run exercises the Queue contract — fenced leases, exclusive claims, expiry
// reclaim, and the attempt lineage that survives Settle — and RunDeltaLog
// exercises the DeltaLog capability. Backends implementing both get the
// fenced-append subtest inside Run via type assertion.
//
// Timings are event-driven where possible: subtests wait on channels with
// generous deadlines rather than sleeping fixed amounts, so the suite stays
// reliable under -race on a loaded machine.
package queuetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
)

const (
	// waitLong bounds events that must happen: a claim that should win, a
	// watcher that should receive. Generous for loaded -race machines.
	waitLong = 5 * time.Second
	// waitShort bounds probes that must find nothing claimable.
	waitShort = 75 * time.Millisecond
)

// claimResult carries a Claim outcome across a goroutine boundary.
type claimResult struct {
	it  queue.Item
	err error
}

// Run exercises a Queue implementation against the full contract. The
// factory must return a fresh, empty queue per call.
func Run(t *testing.T, factory func(t *testing.T) queue.Queue) {
	t.Run("EnqueueThenClaim", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		ttl := time.Second
		before := time.Now()
		it, deadline := mustClaim(t, q, waitLong, ttl)
		after := time.Now()

		if it.RunID != "r1" || it.Attempt != 1 {
			t.Fatalf("Claim = %+v, want {r1 1}", it)
		}
		if deadline.Before(before.Add(ttl)) || deadline.After(after.Add(ttl)) {
			t.Fatalf("deadline %v outside [%v, %v]", deadline, before.Add(ttl), after.Add(ttl))
		}
	})

	t.Run("ClaimExclusivity", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		const claimers = 8
		wins := make(chan queue.Item, claimers)
		losses := make(chan error, claimers)
		var wg sync.WaitGroup
		for range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				it, _, err := q.Claim(ctx, time.Minute)
				if err != nil {
					losses <- err
					return
				}
				wins <- it
			}()
		}
		wg.Wait()
		close(wins)
		close(losses)

		if len(wins) != 1 {
			t.Fatalf("got %d winners, want exactly 1", len(wins))
		}
		if it := <-wins; it.RunID != "r1" || it.Attempt != 1 {
			t.Fatalf("winner = %+v, want {r1 1}", it)
		}
		for err := range losses {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("loser error = %v, want context.DeadlineExceeded", err)
			}
		}
	})

	t.Run("DelayedEnqueue", func(t *testing.T) {
		q := factory(t)
		delay := 250 * time.Millisecond
		notBefore := time.Now().Add(delay)
		mustEnqueue(t, q, "r1", delay)

		// Scheduling may resume after the delay has already elapsed. Check
		// when the successful claim returns instead of assuming a short
		// negative probe always runs before the queue's ready time.
		it, _ := mustClaim(t, q, waitLong, time.Minute)
		if claimedAt := time.Now(); claimedAt.Before(notBefore) {
			t.Fatalf("claimed at %v before enqueue delay ended at %v", claimedAt, notBefore)
		}
		if it.RunID != "r1" || it.Attempt != 1 {
			t.Fatalf("Claim after delay = %+v, want {r1 1}", it)
		}
	})

	t.Run("LeaseExpiry", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		first, _ := mustClaim(t, q, waitLong, 30*time.Millisecond)
		if first.Attempt != 1 {
			t.Fatalf("first claim attempt = %d, want 1", first.Attempt)
		}
		// No heartbeat: the lease lapses and the run is reclaimable.
		second, _ := mustClaim(t, q, 2*time.Second, time.Minute)
		if second.RunID != "r1" || second.Attempt != 2 {
			t.Fatalf("reclaim = %+v, want {r1 2}", second)
		}
	})

	t.Run("HeartbeatExtends", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		// Renew immediately with ample scheduling margin. A repeated ticker
		// against a 120ms lease tests runner scheduling rather than renewal:
		// one delayed tick can legitimately lose ownership on loaded CI.
		it, originalDeadline := mustClaim(t, q, waitLong, 2*time.Second)
		renewed, err := q.Heartbeat(context.Background(), it, 30*time.Second)
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if !renewed.After(originalDeadline) {
			t.Fatalf("renewed deadline %v did not extend %v", renewed, originalDeadline)
		}

		// The competing claim runs past the ORIGINAL expiry. Checking only
		// the returned deadline would miss an adapter that failed to persist it.
		probe, cancel := context.WithDeadline(context.Background(), originalDeadline.Add(waitShort))
		defer cancel()
		competitor, _, err := q.Claim(probe, time.Minute)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("competitor claimed %+v (err=%v) before renewed expiry", competitor, err)
		}

		// Shorten the still-owned lease for the expiry half of the contract.
		// A fresh Claim observes this deadline even for an in-memory backend
		// whose earlier waiting claim had already scheduled a longer timer.
		if _, err := q.Heartbeat(context.Background(), it, 30*time.Millisecond); err != nil {
			t.Fatalf("final Heartbeat: %v", err)
		}
		next, _ := mustClaim(t, q, waitLong, time.Minute)
		if next.RunID != "r1" || next.Attempt != it.Attempt+1 {
			t.Fatalf("competitor = %+v, want {r1 %d}", next, it.Attempt+1)
		}
	})

	t.Run("FencingAfterExpiry", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		old, _ := mustClaim(t, q, waitLong, 30*time.Millisecond)
		fresh, _ := mustClaim(t, q, 2*time.Second, time.Minute) // reclaims after expiry
		if fresh.Attempt != old.Attempt+1 {
			t.Fatalf("reclaim attempt = %d, want %d", fresh.Attempt, old.Attempt+1)
		}

		ctx := context.Background()
		if _, err := q.Heartbeat(ctx, old, time.Minute); !errors.Is(err, run.ErrSuperseded) {
			t.Errorf("stale Heartbeat error = %v, want run.ErrSuperseded", err)
		}
		if err := q.Release(ctx, old, 0, true); !errors.Is(err, run.ErrSuperseded) {
			t.Errorf("stale Release error = %v, want run.ErrSuperseded", err)
		}
		if err := q.Settle(ctx, old); !errors.Is(err, run.ErrSuperseded) {
			t.Errorf("stale Settle error = %v, want run.ErrSuperseded", err)
		}
	})

	t.Run("ReleaseRequeues", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		it, _ := mustClaim(t, q, waitLong, time.Minute)
		delay := 250 * time.Millisecond
		notBefore := time.Now().Add(delay)
		if err := q.Release(context.Background(), it, delay, true); err != nil {
			t.Fatalf("Release: %v", err)
		}

		next, _ := mustClaim(t, q, waitLong, time.Minute)
		if claimedAt := time.Now(); claimedAt.Before(notBefore) {
			t.Fatalf("claimed at %v before release delay ended at %v", claimedAt, notBefore)
		}
		if next.RunID != "r1" || next.Attempt != it.Attempt+1 {
			t.Fatalf("claim after release = %+v, want {r1 %d}", next, it.Attempt+1)
		}
	})

	t.Run("FractionalDelaysDoNotClaimEarly", func(t *testing.T) {
		q := factory(t)
		// A backend with millisecond precision must round the absolute
		// ready time up, including fractions from both the clock and delay.
		delay := waitShort + 500*time.Microsecond
		notBefore := time.Now().Add(delay)
		mustEnqueue(t, q, "r1", delay)
		it, _ := mustClaim(t, q, waitLong, time.Minute)
		if claimedAt := time.Now(); claimedAt.Before(notBefore) {
			t.Fatalf("claimed at %v before fractional enqueue delay ended at %v", claimedAt, notBefore)
		}

		notBefore = time.Now().Add(delay)
		if err := q.Release(context.Background(), it, delay, false); err != nil {
			t.Fatalf("Release: %v", err)
		}
		next, _ := mustClaim(t, q, waitLong, time.Minute)
		if claimedAt := time.Now(); claimedAt.Before(notBefore) {
			t.Fatalf("claimed at %v before fractional release delay ended at %v", claimedAt, notBefore)
		}
		if next.RunID != it.RunID || next.Attempt != it.Attempt+1 || next.Failures != it.Failures {
			t.Fatalf("claim after release = %+v, want same run and failures, next attempt after %+v", next, it)
		}
	})

	t.Run("SettleRemoves", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		it, _ := mustClaim(t, q, waitLong, time.Minute)
		if err := q.Settle(context.Background(), it); err != nil {
			t.Fatalf("Settle: %v", err)
		}

		claimNothing(t, q, waitShort)
	})

	t.Run("ReenqueueAfterSettleContinuesLineage", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		first, _ := mustClaim(t, q, waitLong, time.Minute)
		if err := q.Settle(context.Background(), first); err != nil {
			t.Fatalf("Settle: %v", err)
		}

		// A parked run resuming: the attempt counter must continue, never
		// reset — a reset would let a zombie of the settled attempt pass
		// the fence.
		mustEnqueue(t, q, "r1", 0)
		second, _ := mustClaim(t, q, waitLong, time.Minute)
		if second.RunID != "r1" || second.Attempt != first.Attempt+1 {
			t.Fatalf("claim after re-enqueue = %+v, want {r1 %d}", second, first.Attempt+1)
		}
	})

	t.Run("EnqueueIdempotentWhileLeased", func(t *testing.T) {
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		it, _ := mustClaim(t, q, waitLong, time.Minute)
		mustEnqueue(t, q, "r1", 0) // no-op: the run is leased

		claimNothing(t, q, waitShort)

		if err := q.Release(context.Background(), it, 0, false); err != nil {
			t.Fatalf("Release: %v", err)
		}
		next, _ := mustClaim(t, q, waitLong, time.Minute)
		if next.RunID != "r1" || next.Attempt != it.Attempt+1 {
			t.Fatalf("claim after release = %+v, want {r1 %d}", next, it.Attempt+1)
		}

		// Claimable exactly once: the duplicate enqueue left no second
		// entry behind.
		if err := q.Settle(context.Background(), next); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		claimNothing(t, q, waitShort)
	})

	t.Run("ClaimWakesOnEnqueue", func(t *testing.T) {
		q := factory(t)

		done := make(chan claimResult, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), waitLong)
			defer cancel()
			it, _, err := q.Claim(ctx, time.Minute)
			done <- claimResult{it, err}
		}()

		time.Sleep(30 * time.Millisecond) // let the claimer block first
		mustEnqueue(t, q, "r1", 0)

		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("Claim: %v", r.err)
			}
			if r.it.RunID != "r1" || r.it.Attempt != 1 {
				t.Fatalf("Claim = %+v, want {r1 1}", r.it)
			}
		case <-time.After(waitLong):
			t.Fatal("blocked Claim did not wake on Enqueue")
		}
	})

	t.Run("ClaimHonorsContextCancel", func(t *testing.T) {
		q := factory(t)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, _, err := q.Claim(ctx, time.Minute)
			done <- err
		}()

		time.Sleep(30 * time.Millisecond) // let the claimer block first
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Claim error = %v, want context.Canceled", err)
			}
		case <-time.After(waitLong):
			t.Fatal("blocked Claim did not unblock on cancel")
		}
	})

	t.Run("ClaimOnDoneContextTakesNothing", func(t *testing.T) {
		// A caller that has already stopped could hold no lease, so a
		// Claim on a done context must not take one, even with a run due.
		q := factory(t)
		mustEnqueue(t, q, "r1", 0)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if it, _, err := q.Claim(ctx, time.Minute); err == nil {
			t.Fatalf("Claim on a done context = %+v, want context.Canceled", it)
		} else if !errors.Is(err, context.Canceled) {
			t.Fatalf("Claim error = %v, want context.Canceled", err)
		}

		it, _ := mustClaim(t, q, waitLong, time.Minute)
		if it.RunID != "r1" || it.Attempt != 1 || it.Failures != 0 {
			t.Fatalf("Claim = %+v, want {r1 1 0}: the done-context Claim took a lease", it)
		}
	})

	t.Run("FailureAccounting", func(t *testing.T) {
		// Failures is the retry budget and moves ONLY on real failures:
		// a failed release or an expiry reclaim. Interruptions and
		// park/resume cycles are free — this is what keeps a run that
		// waits on an event from being terminally failed for waiting.
		q := factory(t)
		ctx := context.Background()
		mustEnqueue(t, q, "r1", 0)

		it, _ := mustClaim(t, q, waitLong, time.Minute)
		if it.Failures != 0 {
			t.Fatalf("fresh claim Failures = %d, want 0", it.Failures)
		}

		// Failed release consumes budget.
		if err := q.Release(ctx, it, 0, true); err != nil {
			t.Fatalf("Release(failed): %v", err)
		}
		it, _ = mustClaim(t, q, waitLong, time.Minute)
		if it.Failures != 1 {
			t.Fatalf("Failures after failed release = %d, want 1", it.Failures)
		}

		// Interrupted release (shutdown) is free.
		if err := q.Release(ctx, it, 0, false); err != nil {
			t.Fatalf("Release(interrupted): %v", err)
		}
		it, _ = mustClaim(t, q, waitLong, time.Minute)
		if it.Failures != 1 {
			t.Fatalf("Failures after interrupted release = %d, want 1", it.Failures)
		}

		// Park/resume (Settle then Enqueue) is free; attempt still moves.
		if err := q.Settle(ctx, it); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		mustEnqueue(t, q, "r1", 0)
		resumed, _ := mustClaim(t, q, waitLong, 30*time.Millisecond)
		if resumed.Failures != 1 {
			t.Fatalf("Failures after park/resume = %d, want 1", resumed.Failures)
		}
		if resumed.Attempt != it.Attempt+1 {
			t.Fatalf("attempt after park/resume = %d, want %d", resumed.Attempt, it.Attempt+1)
		}

		// Expiry reclaim consumes budget: the execution died holding the
		// lease (the 30ms ttl above is left to lapse).
		reclaimed, _ := mustClaim(t, q, 2*time.Second, time.Minute)
		if reclaimed.Failures != 2 {
			t.Fatalf("Failures after expiry reclaim = %d, want 2", reclaimed.Failures)
		}
	})

	t.Run("FencedAppend", func(t *testing.T) {
		q := factory(t)
		log, ok := q.(queue.DeltaLog)
		if !ok {
			t.Skip("queue does not implement queue.DeltaLog")
		}
		mustEnqueue(t, q, "r1", 0)

		// Claim at attempt 1, let it expire, reclaim at attempt 2.
		mustClaim(t, q, waitLong, 30*time.Millisecond)
		it, _ := mustClaim(t, q, 2*time.Second, time.Minute)
		if it.Attempt != 2 {
			t.Fatalf("reclaim attempt = %d, want 2", it.Attempt)
		}

		ctx := context.Background()
		stale := run.Delta{RunID: "r1", Attempt: 1, Kind: run.DeltaUser, Payload: []byte("zombie")}
		if err := log.Append(ctx, stale); !errors.Is(err, run.ErrSuperseded) {
			t.Errorf("stale Append error = %v, want run.ErrSuperseded", err)
		}
		fresh := run.Delta{RunID: "r1", Attempt: 2, Kind: run.DeltaUser, Payload: []byte("current")}
		if err := log.Append(ctx, fresh); err != nil {
			t.Errorf("current Append: %v", err)
		}
	})
}

// RunDeltaLog exercises a DeltaLog implementation. The factory must return a
// fresh, empty log per call. Fenced appends are covered by Run when the
// backend also implements Queue.
func RunDeltaLog(t *testing.T, factory func(t *testing.T) queue.DeltaLog) {
	t.Run("AppendThenRead", func(t *testing.T) {
		log := factory(t)
		ctx := context.Background()
		appendDeltas(t, log, "r1", "a", "b", "c")

		first, cur, err := log.Read(ctx, "r1", "", 2)
		if err != nil {
			t.Fatalf("Read from start: %v", err)
		}
		wantPayloads(t, first, "a", "b")

		rest, cur2, err := log.Read(ctx, "r1", cur, 10)
		if err != nil {
			t.Fatalf("Read from %q: %v", cur, err)
		}
		wantPayloads(t, rest, "c")
		if cur2 == cur {
			t.Fatalf("cursor did not advance past %q", cur)
		}

		tail, _, err := log.Read(ctx, "r1", cur2, 10)
		if err != nil {
			t.Fatalf("Read from %q: %v", cur2, err)
		}
		wantPayloads(t, tail)
	})

	t.Run("WatchCatchUpThenLive", func(t *testing.T) {
		log := factory(t)
		ctx, cancel := context.WithTimeout(context.Background(), waitLong)
		defer cancel()
		appendDeltas(t, log, "r1", "a", "b")

		// Two concurrent watchers: each must see every delta.
		watchers := make([]queue.Stream, 2)
		for i := range watchers {
			s, err := log.Watch(ctx, "r1", "")
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			watchers[i] = s
			for _, want := range []string{"a", "b"} {
				d, _, err := s.Recv(ctx)
				if err != nil {
					t.Fatalf("watcher %d catch-up Recv: %v", i, err)
				}
				if string(d.Payload) != want {
					t.Fatalf("watcher %d caught up %q, want %q", i, d.Payload, want)
				}
			}
		}

		// Live tail: block both watchers on Recv, then append.
		results := make([]chan claimRecv, len(watchers))
		for i, s := range watchers {
			ch := make(chan claimRecv, 1)
			results[i] = ch
			go func() {
				d, _, err := s.Recv(ctx)
				ch <- claimRecv{d, err}
			}()
		}
		time.Sleep(50 * time.Millisecond) // let both Recvs block first
		appendDeltas(t, log, "r1", "c")

		for i, ch := range results {
			select {
			case r := <-ch:
				if r.err != nil {
					t.Fatalf("watcher %d live Recv: %v", i, r.err)
				}
				if string(r.d.Payload) != "c" {
					t.Fatalf("watcher %d live delta %q, want %q", i, r.d.Payload, "c")
				}
			case <-time.After(waitLong):
				t.Fatalf("watcher %d never received the live append", i)
			}
		}
	})

	t.Run("WatchHonorsContextCancel", func(t *testing.T) {
		log := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		s, err := log.Watch(ctx, "r1", "")
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := s.Recv(ctx)
			done <- err
		}()

		time.Sleep(30 * time.Millisecond) // let Recv block first
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Recv error = %v, want context.Canceled", err)
			}
		case <-time.After(waitLong):
			t.Fatal("blocked Recv did not unblock on cancel")
		}
	})

	t.Run("TrimEmpties", func(t *testing.T) {
		log := factory(t)
		ctx := context.Background()
		appendDeltas(t, log, "r1", "a", "b")

		if err := log.Trim(ctx, "r1"); err != nil {
			t.Fatalf("Trim: %v", err)
		}
		got, _, err := log.Read(ctx, "r1", "", 10)
		if err != nil {
			t.Fatalf("Read after Trim: %v", err)
		}
		wantPayloads(t, got)
	})
}

// claimRecv carries a Recv outcome across a goroutine boundary.
type claimRecv struct {
	d   run.Delta
	err error
}

// mustEnqueue enqueues or fails the test.
func mustEnqueue(t *testing.T, q queue.Queue, runID string, delay time.Duration) {
	t.Helper()
	if err := q.Enqueue(context.Background(), runID, delay); err != nil {
		t.Fatalf("Enqueue(%q, %v): %v", runID, delay, err)
	}
}

// mustClaim claims within timeout or fails the test.
func mustClaim(t *testing.T, q queue.Queue, timeout, ttl time.Duration) (queue.Item, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	it, deadline, err := q.Claim(ctx, ttl)
	if err != nil {
		t.Fatalf("Claim(ttl=%v): %v", ttl, err)
	}
	return it, deadline
}

// claimNothing asserts that nothing is claimable within timeout.
func claimNothing(t *testing.T, q queue.Queue, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	it, _, err := q.Claim(ctx, time.Minute)
	if err == nil {
		t.Fatalf("Claim returned %+v, want nothing claimable", it)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Claim error = %v, want context.DeadlineExceeded", err)
	}
}

// appendDeltas appends one user delta per payload, all at attempt 1.
func appendDeltas(t *testing.T, log queue.DeltaLog, runID string, payloads ...string) {
	t.Helper()
	for _, p := range payloads {
		d := run.Delta{RunID: runID, Attempt: 1, Kind: run.DeltaUser, Payload: []byte(p)}
		if err := log.Append(context.Background(), d); err != nil {
			t.Fatalf("Append(%q): %v", p, err)
		}
	}
}

// wantPayloads asserts got carries exactly the given payloads in order.
func wantPayloads(t *testing.T, got []run.Delta, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d deltas, want %d", len(got), len(want))
	}
	for i, w := range want {
		if string(got[i].Payload) != w {
			t.Fatalf("delta %d payload = %q, want %q", i, got[i].Payload, w)
		}
	}
}
