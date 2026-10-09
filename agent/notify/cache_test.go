package notify

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCacheInvalidatesOtherLocals(t *testing.T) {
	ctx := context.Background()
	n := NewMemory(0)
	defer n.Close()
	shared := memcache.New[string]()
	localA, localB := memcache.New[string](), memcache.New[string]()
	a, err := NewCache(ctx, CacheConfig[string]{Local: localA, Shared: shared, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewCache(ctx, CacheConfig[string]{Local: localB, Shared: shared, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.Set(ctx, "k", "v1", 0); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := b.Get(ctx, "k"); !ok || v != "v1" {
		t.Fatalf("b.Get = %q %v", v, ok)
	}
	// A's invalidation may reach b after that read and evict the fill, so
	// read until the fill sticks.
	eventually(t, func() bool {
		_, _, _ = b.Get(ctx, "k")
		_, ok, _ := localB.Get(ctx, "k")
		return ok
	})

	// A's write must evict B's stale local copy, and A's own local copy
	// must survive A's own notification.
	if err := a.Set(ctx, "k", "v2", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, ok, _ := localB.Get(ctx, "k"); return !ok })
	if v, ok, _ := b.Get(ctx, "k"); !ok || v != "v2" {
		t.Fatalf("b.Get after update = %q %v", v, ok)
	}
	if _, ok, _ := localA.Get(ctx, "k"); !ok {
		t.Fatal("a evicted its own write")
	}

	if err := b.Invalidate(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, ok, _ := localA.Get(ctx, "k"); return !ok })
	if _, ok, _ := a.Get(ctx, "k"); ok {
		t.Fatal("invalidated key still readable")
	}
}

func TestCacheBypassesLocalAfterNotifierCloses(t *testing.T) {
	ctx := context.Background()
	n := NewMemory(0)
	shared, local := memcache.New[string](), memcache.New[string]()
	c, err := NewCache(ctx, CacheConfig[string]{Local: local, Shared: shared, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Set(ctx, "k", "v1", 0); err != nil {
		t.Fatal(err)
	}
	_ = n.Close()
	eventually(t, c.degraded.Load)
	// Another writer changes the shared level without a notification.
	_ = shared.Set(ctx, "k", "v2", 0)
	if v, _, _ := c.Get(ctx, "k"); v != "v2" {
		t.Fatalf("Get = %q, want the shared value", v)
	}
}

func TestCacheRequiresChannel(t *testing.T) {
	_, err := NewCache(context.Background(), CacheConfig[int]{Local: memcache.New[int](), Notifier: NewMemory(0), Channel: "bad channel"})
	if err == nil {
		t.Fatal("invalid channel accepted")
	}
	var _ types.Cache[int] = (*Cache[int])(nil)
}
