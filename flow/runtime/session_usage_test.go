package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
)

type unavailableUsageReceipts struct {
	*ledger.Memory
	err error
}

func (l unavailableUsageReceipts) RecordsWithPrefix(ctx context.Context, _ string, _ ...string) ([]run.Record, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, l.err
}

func TestSessionUsageReconciliationPreservesAttemptCancellation(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "canceled"
		if expired {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			store := session.NewMemory()
			rec, err := store.Open(context.Background(), "t", session.Open{Parent: "console://t/root", Agent: "lead"})
			if err != nil {
				t.Fatal(err)
			}
			l := unavailableUsageReceipts{Memory: ledger.NewMemory()}
			rt := &Runtime{sessions: store, stops: l, reader: l}
			err = rt.reconcileSessionUsage(ctx, Input{Trigger: event.Envelope{Tenant: "t", ID: "request"}, Session: &rec})
			if !errors.Is(err, want) || errors.Is(err, run.ErrParked) {
				t.Fatalf("cancelled reconciliation = %v; want %v without parking", err, want)
			}
		})
	}
}

func TestSessionUsageReconciliationParksAnIndependentReceiptFailure(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemory()
	rec, err := store.Open(ctx, "t", session.Open{Parent: "console://t/root", Agent: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	l := unavailableUsageReceipts{Memory: ledger.NewMemory(), err: errors.New("receipt storage unavailable")}
	rt := &Runtime{sessions: store, stops: l, reader: l}
	err = rt.reconcileSessionUsage(ctx, Input{Trigger: event.Envelope{Tenant: "t", ID: "request"}, Session: &rec})
	if !errors.Is(err, run.ErrParked) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transient receipt failure = %v; want parking for durable retry", err)
	}
}
