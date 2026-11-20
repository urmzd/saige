package filequeue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/filequeue"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/queue/queuetest"
)

func TestConformance(t *testing.T) {
	queuetest.Run(t, func(t *testing.T) queue.Queue {
		q, err := filequeue.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return q
	})
}

// Two queues on one directory are two processes: one leases a run, the
// other cannot claim it, sees it leased, and gets it back as soon as the
// first lets go of its lock, before the lease's deadline.
func TestALeaseIsSharedAcrossProcessesAndFreedWithItsHolder(t *testing.T) {
	dir := t.TempDir()
	a, _ := filequeue.New(dir)
	b, _ := filequeue.New(dir)
	ctx := context.Background()
	if err := a.Enqueue(ctx, "r1", 0); err != nil {
		t.Fatal(err)
	}
	it, _, err := a.ClaimRun(ctx, "r1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.ClaimRun(ctx, "r1", time.Hour); !errors.Is(err, queue.ErrLeased) {
		t.Fatalf("second claimer: %v, want ErrLeased", err)
	}
	if leased, err := b.Leased(ctx, "r1"); err != nil || !leased {
		t.Fatalf("leased = %v, %v", leased, err)
	}
	if err := a.Release(ctx, it, 0, false); err != nil {
		t.Fatal(err)
	}
	again, _, err := b.ClaimRun(ctx, "r1", time.Hour)
	if err != nil || again.Attempt != 2 || again.Failures != 0 {
		t.Fatalf("after release: %+v, %v", again, err)
	}
	if _, _, err := b.ClaimRun(ctx, "unknown", time.Hour); !errors.Is(err, queue.ErrNotQueued) {
		t.Fatalf("unknown run: %v, want ErrNotQueued", err)
	}
}
