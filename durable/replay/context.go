package replay

import "context"

type ctxKey struct{}

// WithFrame returns a context carrying the frame. The worker installs it
// before invoking the registered workflow function; wrapped calls in user
// code find it again through the plain context.Context they already thread.
func WithFrame(ctx context.Context, f *Frame) context.Context {
	return context.WithValue(ctx, ctxKey{}, f)
}

// FromContext returns the frame, if the context is executing under a run.
// Absent a frame, wrapped calls pass through as plain Go calls — the
// property that makes adoption a wrapping exercise, not a migration.
func FromContext(ctx context.Context) (*Frame, bool) {
	f, ok := ctx.Value(ctxKey{}).(*Frame)
	return f, ok
}

type activeKey struct{}

// withActive returns a context whose wrapped calls nest under key.
func withActive(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, activeKey{}, key)
}

// ActiveKey returns the record key of the wrapped call ctx is executing
// inside, or "" outside one. Combined with the run ID it is the idempotency
// key handed to downstream systems: "{runID}:{key}". The key travels in the
// context, so wrapped calls running at once on separate goroutines each see
// their own.
func ActiveKey(ctx context.Context) string {
	key, _ := ctx.Value(activeKey{}).(string)
	return key
}
