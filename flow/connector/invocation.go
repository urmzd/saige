package connector

import "context"

type runIdentityKey struct{}

// WithRunID binds a tool call to the durable run that executes it. Composition
// code sets this value; model arguments and source-address parameters cannot.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIdentityKey{}, runID)
}

// RunID returns the composition-bound durable run identity, or empty outside a run.
func RunID(ctx context.Context) string { id, _ := ctx.Value(runIdentityKey{}).(string); return id }
