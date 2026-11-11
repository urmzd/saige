// Package queue defines the flow-and-clock interface: delivery of run IDs to
// workers under time-bounded, fenced claims. The queue carries only IDs —
// data lives in the ledger — and every queue backend is disposable: its
// entire content is derivable from the ledger, so wiping it loses no runs.
//
// Lifecycle per claim: claim → heartbeat* → exactly one of settle, release,
// expire. Delivery is at-least-once; exactly-once observation is
// manufactured above by the ledger's first-write-wins records and
// first-terminal-wins Complete.
package queue

import (
	"context"
	"time"
)

// Item is a claimed run. The (RunID, Attempt) pair is the fencing token for
// every subsequent operation on the claim; Failures is the run's retry
// budget consumption. The two are deliberately separate counters: Attempt
// increments on every claim (fencing must be monotonic across the run's
// whole lineage, including park/resume cycles), while Failures counts only
// executions that actually failed — a voluntary Release back onto the queue
// or a lease that expired under its worker. Settling and re-enqueueing (how
// parked runs wait) never touches Failures, so waiting is free.
type Item struct {
	RunID    string
	Attempt  int
	Failures int
}

// Queue is durable's delivery primitive. Implementations must satisfy the
// conformance suite in queuetest.
type Queue interface {
	// Enqueue makes runID claimable after delay (0 = immediately).
	// Idempotent: enqueueing a run that is already queued or claimed is a
	// no-op, so duplicate enqueues (janitor, resubmission) are harmless.
	Enqueue(ctx context.Context, runID string, delay time.Duration) error

	// Claim blocks until a run is claimable or ctx is done, then hands it
	// to exactly one caller with a lease of ttl. The returned Item carries
	// the incremented attempt and the run's accumulated failure count; the
	// time is the lease deadline. Expiry is queue-internal: a lapsed lease
	// makes the run claimable again, and reclaiming it increments both the
	// attempt and — because the previous execution died — Failures.
	//
	// A caller may stop mid-claim. Called with a done ctx, Claim takes
	// nothing and returns ctx's error. Once a claim has been sent, though,
	// the store can grant its lease to a caller that stopped listening, so
	// a backend should wait out the answer to a claim already sent, for a
	// bounded time, rather than abandon it when ctx is done, and return the
	// lease it took. Claim may therefore return an Item after ctx is done:
	// the caller holds that lease like any other. A caller that is stopping
	// hands it back unexecuted with Release on a context the stop does not
	// cancel, such as context.WithoutCancel(ctx), with delay 0 and
	// failed=false. ctx itself is done, and a backend may refuse a call on
	// a done context (pgqueue does), which leaves the lease to
	// lapse and the reclaim to charge the run a failure.
	//
	// An error from Claim, ctx's own included, does not prove that no lease
	// was taken. Once a claim is sent, a lost answer (the store silent past
	// the backend's bound, a connection broken after the store took the
	// lease, a client that stopped waiting) leaves the outcome unknown: a
	// lease taken then belongs to nobody until it lapses after ttl, and the
	// reclaim charges the run a failure, as for a crashed worker. Memory
	// claims under its lock and loses no answer. pgqueue waits up to 5s
	// past ctx's end for a claim statement already sent.
	Claim(ctx context.Context, ttl time.Duration) (Item, time.Time, error)

	// Heartbeat extends the lease by ttl iff it still holds. A stale
	// attempt gets run.ErrSuperseded — how a zombie learns it lost.
	Heartbeat(ctx context.Context, it Item, ttl time.Duration) (time.Time, error)

	// Release gives the run back voluntarily, claimable again after delay.
	// Fenced by it.Attempt. failed says whether this execution consumed
	// retry budget: true after a retryable error (Failures increments),
	// false when the worker is merely interrupted (graceful shutdown) —
	// interruption is not a failure of the code, and deploys must not burn
	// the budget of long-running work.
	Release(ctx context.Context, it Item, delay time.Duration, failed bool) error

	// Settle removes the run from the queue entirely: after a terminal
	// Complete, or when a run parks. Fenced by it.Attempt.
	Settle(ctx context.Context, it Item) error
}
