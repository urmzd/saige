package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/session"
)

// reconcileSessionUsage runs after a memoized model activity. Only retained
// per-call receipts are replayed; child totals and uncertain spend are never
// synthesized. A temporary index failure parks at this step, so the janitor
// can retry after restart without executing the completed model activity again.
func (r *Runtime) reconcileSessionUsage(ctx context.Context, in Input) error {
	store, ok := r.sessions.(session.UsageStore)
	if !ok || in.Session == nil || r.stops == nil {
		return nil
	}
	_, records, err := r.recordsOf(ctx, in.runID(), false, prefixUsage)
	if err != nil {
		return pendingUsage(ctx, err)
	}
	// Receipts are append-only. Key each effect by the immutable actual call,
	// not one aggregate checkpoint that would hide calls from later attempts.
	for _, receipt := range usageRecords(records) {
		for _, call := range receipt.Calls {
			key := fmt.Sprintf("fiber.session.usage.v1:%x", sha256.Sum256([]byte(call.AccountingID)))
			_, err := durable.Step(ctx, key, func(ctx context.Context) (struct{}, error) {
				err := store.RecordUsage(ctx, in.Trigger.Tenant, in.Session.ID, call)
				if errors.Is(err, session.ErrInvalid) || errors.Is(err, session.ErrNotFound) {
					err = durable.NonRetryable(err)
				}
				return struct{}{}, err
			})
			if err != nil {
				if run.IsNonRetryable(err) || errors.Is(err, run.ErrNonDeterministic) {
					return err
				}
				return pendingUsage(ctx, err)
			}
		}
	}
	return nil
}

func pendingUsage(ctx context.Context, err error) error {
	// Shutdown must release the claimed run immediately. Parking instead would
	// settle its queue entry and leave recovery waiting for the janitor.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("runtime: session usage receipt reconciliation pending (%v): %w", err, run.ErrParked)
}
