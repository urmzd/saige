package durable

import (
	"context"

	"github.com/urmzd/saige/durable/replay"
)

// RunInfo identifies the run a context is executing under.
type RunInfo struct {
	RunID   string
	Attempt int
}

// FromContext returns the run identity, if ctx is executing under a run.
func FromContext(ctx context.Context) (RunInfo, bool) {
	f, ok := replay.FromContext(ctx)
	if !ok {
		return RunInfo{}, false
	}
	return RunInfo{RunID: f.RunID(), Attempt: f.Attempt()}, true
}

// InRun reports whether ctx is executing under a run.
func InRun(ctx context.Context) bool {
	_, ok := replay.FromContext(ctx)
	return ok
}

// IdempotencyKey returns "{runID}:{recordKey}" for the activity currently
// executing — a stable key to hand downstream systems (payment providers,
// mail APIs) so durable's at-least-once side effects become effective-once
// where the callee deduplicates. Empty outside a wrapped call.
func IdempotencyKey(ctx context.Context) string {
	f, ok := replay.FromContext(ctx)
	if !ok {
		return ""
	}
	key := f.ActiveKey()
	if key == "" {
		return ""
	}
	return f.RunID() + ":" + key
}
