// Package run defines durable's leaf types: runs, records, deltas, and the
// sentinel errors every layer speaks. It is stdlib-only, imported by every
// other package, and imports none of them.
//
// A run is one durable invocation of a wrapped workflow function. A record is
// one memoized result inside a run — an activity checkpoint, a captured step
// of non-determinism, or an event. Record identity is (RunID, Key) where Key
// follows the activity, not its position in the code: the Kth call to
// "charge-payment" in program order is "charge-payment#2" (zero-indexed
// occurrences), an explicit user key is "k:<key>", and an event is
// "event:<name>#<k>". Identity-by-name is what lets code evolve freely against
// records that never change.
package run

import (
	"errors"
	"time"
)

// RunStatus is a run's lifecycle state. There is no "running" status:
// assignment is flow state and lives only in the queue, never in the ledger.
type RunStatus string

// A run is pending until it completes, then succeeded or failed.
const (
	StatusPending   RunStatus = "pending"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
)

// Terminal reports whether s is a final state.
func (s RunStatus) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed
}

// Kind classifies a record.
type Kind string

const (
	// KindActivity is a wrapped side-effecting call, checkpointed on success.
	KindActivity Kind = "activity"
	// KindStep is captured non-determinism (time, rand, uuid); the first
	// recorded value is the value forever.
	KindStep Kind = "step"
	// KindEvent is a record written from outside the run (a signal, an
	// approval row, a matured timer). Waiting is the absence of one.
	KindEvent Kind = "event"
)

// RecordStatus is a record's terminal state. Records are write-once: a
// retryable failure writes nothing (the next attempt re-executes); only
// successes and non-retryable failures are recorded.
type RecordStatus string

// A record holds a recorded result or a non-retryable failure.
const (
	RecordOK     RecordStatus = "recorded"
	RecordFailed RecordStatus = "failed"
)

// Run is one durable invocation of a workflow function.
type Run struct {
	ID          string // caller-supplied (doubles as the submit idempotency key) or generated
	Name        string // registered workflow name
	Input       []byte
	Status      RunStatus
	Attempt     int // attempt that wrote the terminal result; audit only
	MaxAttempts int // retry budget: maximum FAILED executions; waiting is free
	Output      []byte
	Error       string
	ParentRunID string // lineage seam: forks and child runs; informational in v1
	CreatedAt   time.Time
	CompletedAt time.Time
}

// Result is the terminal outcome of a run.
type Result struct {
	Status RunStatus // StatusSucceeded or StatusFailed
	Output []byte
	Error  string
}

// Record is one memoized result inside a run.
//
// Keys are hierarchical: a call made inside another wrapped call is scoped
// under its parent's key ("process-order#0/charge-payment#0"), so a memoized
// parent — whose body never re-executes — cannot desynchronize its siblings'
// occurrence counters.
type Record struct {
	RunID      string
	Key        string // "name#k" | "k:<user key>" | "event:<name>#<k>", each segment optionally under "parentkey/"
	Kind       Kind
	Name       string
	InputHash  string // sha256 hex of the marshaled input; "" for steps and events
	Codec      string // Codec.ContentType() that produced Output; verified on replay
	Status     RecordStatus
	Output     []byte
	Error      string // set when Status == RecordFailed
	Attempt    int    // run attempt that recorded it; audit only
	RecordedAt time.Time
}

// PendingRun is one janitor-visible pending run: the ID plus the creation
// time that orders and paginates the scan.
type PendingRun struct {
	ID        string
	CreatedAt time.Time
}

// PendingCursor pages through pending runs in (CreatedAt, ID) order. The
// zero value starts from the beginning; pass the last row of a batch to get
// the next one.
type PendingCursor struct {
	CreatedAt time.Time
	ID        string
}

// DeltaKind classifies an entry in a run's delta log.
type DeltaKind string

const (
	// DeltaUser is a payload emitted by user code during an activity.
	DeltaUser DeltaKind = "user"
	// DeltaAttempt is a framework-written divider: attempt N started
	// executing. Consumers split segments of superseded attempts here.
	DeltaAttempt DeltaKind = "attempt"
	// DeltaSeal is a framework-written divider: the record named by
	// RecordKey was checkpointed; its segment is resolved by the ledger.
	DeltaSeal DeltaKind = "seal"
)

// Delta is one entry in a run's stream: advisory flow, never truth. Deltas
// are sealed or superseded by checkpoints; losing them loses observability
// history, never correctness.
type Delta struct {
	RunID     string
	RecordKey string // record being executed when emitted; "" for attempt dividers
	Attempt   int
	Kind      DeltaKind
	Payload   []byte
}

// Sentinel errors shared across ledgers, queues, and replay. Wrap with
// fmt.Errorf("pkg: context: %w", err) and test with errors.Is.
var (
	// ErrNotFound reports a run the ledger does not hold.
	ErrNotFound = errors.New("durable: run not found")

	// ErrSuperseded is the zombie's answer: the (RunID, Attempt) fencing
	// pair presented is no longer current. Heartbeats, releases, settles,
	// and delta appends from a stale attempt all receive it.
	ErrSuperseded = errors.New("durable: attempt superseded")

	// ErrAlreadyTerminal reports a Complete against a run that already has
	// a terminal result. First terminal write wins; later ones are dropped.
	ErrAlreadyTerminal = errors.New("durable: run already terminal")

	// ErrAlreadyRecorded reports a Record conflict: a record with the same
	// (RunID, Key) exists. First write wins; the caller must adopt the
	// stored record returned alongside this error.
	ErrAlreadyRecorded = errors.New("durable: record already exists")

	// ErrNonDeterministic reports replay divergence: a memoized record's
	// kind or input hash does not match the live call. Terminal and
	// non-retryable — identical code against identical records cannot
	// succeed, so failing fast is the honest move.
	ErrNonDeterministic = errors.New("durable: non-deterministic replay")

	// ErrParked reports that a run reached an Event that has no record yet.
	// The run just stops: the worker settles the queue item and the run
	// stays pending until something writes the event record and enqueues
	// the run again. User code propagates it like any error.
	ErrParked = errors.New("durable: run parked awaiting event")

	// ErrMaxAttempts reports a run that exhausted its retry budget.
	ErrMaxAttempts = errors.New("durable: max attempts exhausted")
)
