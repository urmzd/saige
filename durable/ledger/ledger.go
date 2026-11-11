// Package ledger defines the durable-truth interface: where runs and their
// memoized records live. Any store that can do an idempotent keyed insert, a
// conditional pending→terminal update, and a point read qualifies — your
// existing Postgres table, SQLite, DynamoDB, or the in-process Memory
// implementation here. durable never mandates a schema: adapters map onto
// storage the caller already owns and validate rather than migrate.
//
// Durable writes per run on the happy path: exactly two run-level writes
// (Accept, Complete) plus one Record per completed activity or step.
// Heartbeats never touch the ledger — the clock lives in the queue.
package ledger

import (
	"context"
	"time"

	"github.com/urmzd/saige/durable/run"
)

// Ledger is durable's source of truth. Implementations must satisfy the
// conformance suite in ledgertest.
type Ledger interface {
	// Accept durably registers a run. Idempotent: accepting an existing
	// run ID is a no-op returning nil, which makes the run ID a submit
	// idempotency key and makes replayed submissions safe.
	Accept(ctx context.Context, r run.Run) error

	// Complete writes the terminal result: a CAS that transitions
	// pending → succeeded|failed exactly once. The first terminal write
	// wins, from any attempt; a later Complete gets run.ErrAlreadyTerminal
	// and a missing run gets run.ErrNotFound. attempt is stored for audit,
	// not checked — a superseded attempt that finishes first computed the
	// same memoized prefix and its result is valid.
	Complete(ctx context.Context, runID string, attempt int, res run.Result) error

	// Load returns the run and all of its records — the replay prefetch,
	// one round trip at claim time. Record order is not part of the
	// contract; replay addresses records by key.
	Load(ctx context.Context, runID string) (run.Run, []run.Record, error)

	// Record persists one memoized result, write-once. On a (RunID, Key)
	// conflict it returns the already-stored record and
	// run.ErrAlreadyRecorded: first write wins, and the caller must adopt
	// the stored record so concurrent attempts converge on one history.
	Record(ctx context.Context, rec run.Record) (run.Record, error)

	// ListPending returns non-terminal runs created before t, in
	// (CreatedAt, ID) ascending order, strictly after the cursor (zero
	// cursor = from the beginning), up to limit. This is the janitor seam:
	// it re-enqueues accepted-but-lost runs, doubles as the backstop poll
	// for parked runs, and rebuilds a wiped queue from truth. The cursor
	// exists so a janitor can walk an arbitrarily large pending set —
	// parked runs stay pending indefinitely and must not starve newer
	// lost runs out of a fixed first page.
	ListPending(ctx context.Context, before time.Time, cursor run.PendingCursor, limit int) ([]run.PendingRun, error)
}

// RunGetter is an optional capability: a point read of one run without its
// records. Result polling and status checks prefer it — Load prefetches the
// full record history, which is the right cost at claim time and the wrong
// cost in a poll loop. Discovered by type assertion, like queue.DeltaLog.
type RunGetter interface {
	GetRun(ctx context.Context, runID string) (run.Run, error)
}

// PrefixReader is an optional capability: the records of one run whose keys
// start with one of prefixes. A poll that wants a few small records (an
// event's arrival, a stop request) prefers it to Load, which reads every
// record of the run, a large transcript among them. Discovered by type
// assertion, like RunGetter.
//
// Records come back in no particular order, each once even when it matches
// several prefixes. A prefix is literal and case-sensitive, compared byte by
// byte like strings.HasPrefix: no wildcards. Nested records share their
// parent's key as a prefix (process-order#0/charge-payment#0), so a parent's
// key reads its whole subtree. No prefixes match nothing; an empty prefix
// matches every record. An unknown run is run.ErrNotFound, as in Load.
type PrefixReader interface {
	RecordsWithPrefix(ctx context.Context, runID string, prefixes ...string) ([]run.Record, error)
}
