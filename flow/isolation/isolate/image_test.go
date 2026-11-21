package isolate_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/isolate"

	"github.com/urmzd/saige/flow/isolation"
	poolleaser "github.com/urmzd/saige/flow/isolation/isolate"
)

// specDriver records the spec of each start and starts nothing.
type specDriver struct {
	mu    sync.Mutex
	specs []isolate.Spec
}

func (d *specDriver) Start(_ context.Context, spec isolate.Spec) (isolate.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	return nil, fmt.Errorf("%w: recorded", isolate.ErrStart)
}

// A request's image replaces the Leaser's for that lease only; an empty one
// leases the Leaser's, and a malformed one is refused before any start.
func TestLeaseUsesTheRequestsImage(t *testing.T) {
	d := &specDriver{}
	l, err := poolleaser.New(d, poolleaser.Config{Image: "base:1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, req := range []isolation.Request{
		{Run: "r1"},
		{Run: "r2", Image: "ghcr.io/acme/agent@sha256:abc"},
		{Run: "r3", Image: "ghcr.io/acme/agent@sha256:abc", Connectors: true},
	} {
		if _, err := l.Lease(ctx, req); !errors.Is(err, isolate.ErrStart) {
			t.Fatalf("lease %s: %v, want the recorded start", req.Run, err)
		}
	}
	got := make([]string, 0, len(d.specs))
	for _, s := range d.specs {
		got = append(got, s.Image)
	}
	want := []string{"base:1", "ghcr.io/acme/agent@sha256:abc", "ghcr.io/acme/agent@sha256:abc"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("started images %v, want %v", got, want)
	}
	if _, err := l.Lease(ctx, isolation.Request{Run: "r4", Image: "-v /:/host"}); !errors.Is(err, isolate.ErrInvalidSpec) {
		t.Fatalf("a malformed image: %v, want ErrInvalidSpec", err)
	}
	if len(d.specs) != 3 {
		t.Fatalf("a malformed image reached the driver: %d starts", len(d.specs))
	}
}

// Warm starts the request's image before any lease, and refuses a malformed one.
func TestWarmStartsTheRequestsImage(t *testing.T) {
	d := &specDriver{}
	l, err := poolleaser.New(d, poolleaser.Config{Image: "base:1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	if err := l.Warm(isolation.Request{Image: "-bad"}); !errors.Is(err, isolate.ErrInvalidSpec) {
		t.Fatalf("a malformed image: %v, want ErrInvalidSpec", err)
	}
	if err := l.Warm(isolation.Request{Image: "ghcr.io/acme/agent@sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.specs)
		var image string
		if n > 0 {
			image = d.specs[0].Image
		}
		d.mu.Unlock()
		if n > 0 {
			if image != "ghcr.io/acme/agent@sha256:abc" {
				t.Fatalf("warmed %q", image)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Warm started nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
