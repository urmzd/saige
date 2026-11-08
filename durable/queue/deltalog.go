package queue

import (
	"context"

	"github.com/urmzd/saige/durable/run"
)

// Cursor is an opaque, backend-specific position in a run's delta log.
// Empty means "from the beginning".
type Cursor string

// Stream delivers a run's deltas in append order: recorded history first,
// then live tail. Recv blocks until a delta is available or ctx is done.
type Stream interface {
	Recv(ctx context.Context) (run.Delta, Cursor, error)
}

// DeltaLog is an optional capability of a queue backend, discovered by type
// assertion — the template for every adapter-specific power. It holds the
// stream an activity produces while executing: advisory flow, sealed or
// superseded by ledger checkpoints, never truth.
//
// Framework-written dividers (run.DeltaAttempt, run.DeltaSeal) let consumers
// split segments by attempt and discard superseded partial output; see the
// consumer algorithm in docs/architecture/replay.md.
type DeltaLog interface {
	// Append adds one delta. Appends are fenced like heartbeats: a delta
	// carrying a superseded attempt gets run.ErrSuperseded.
	Append(ctx context.Context, d run.Delta) error

	// Watch streams a run's deltas from a cursor: catch-up, then live
	// tail until ctx is done.
	Watch(ctx context.Context, runID string, from Cursor) (Stream, error)

	// Read returns a bounded snapshot of a run's deltas from a cursor —
	// no live tail. Used to reconstruct history (a retrying activity
	// reading its prior attempt's segment, a UI splitting on dividers).
	Read(ctx context.Context, runID string, from Cursor, limit int) ([]run.Delta, Cursor, error)

	// Trim discards a run's log (retention is caller policy; the ledger
	// keeps the truth).
	Trim(ctx context.Context, runID string) error
}
