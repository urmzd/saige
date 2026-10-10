package notify

import (
	"context"
	"sync"
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
	defer n.Close(ctx)
	shared := memcache.New[string]()
	localA, localB := memcache.New[string](), memcache.New[string]()
	a, err := NewCache(ctx, CacheConfig[string]{Local: localA, Shared: shared, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	b, err := NewCache(ctx, CacheConfig[string]{Local: localB, Shared: shared, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)

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
	defer c.Close(ctx)
	if err := c.Set(ctx, "k", "v1", 0); err != nil {
		t.Fatal(err)
	}
	_ = n.Close(ctx)
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

// hookedCache runs afterSet once, after the first Set reaches it.
type hookedCache struct {
	types.Cache[string]
	once     sync.Once
	afterSet func()
}

func (h *hookedCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := h.Cache.Set(ctx, key, value, ttl); err != nil {
		return err
	}
	h.once.Do(h.afterSet)
	return nil
}

// A write from another process that lands between Set's shared write and
// its local write must not leave the older value in Local. Set once wrote
// Local unconditionally, so the process kept serving its own, overwritten
// value.
func TestCacheSetRacingRemoteWrite(t *testing.T) {
	ctx := context.Background()
	n := NewMemory(0)
	defer n.Close(ctx)
	store := memcache.New[string]()
	localA, localB := memcache.New[string](), memcache.New[string]()
	var a *Cache[string]
	b, err := NewCache(ctx, CacheConfig[string]{Local: localB, Shared: store, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	hooked := &hookedCache{Cache: store}
	hooked.afterSet = func() {
		// B overwrites the key after A's shared write, and its
		// invalidation reaches A before A writes Local.
		if err := b.Set(ctx, "k", "from-b", 0); err != nil {
			t.Error(err)
		}
		eventually(t, func() bool { return a.generation.Load() > 0 })
	}
	a, err = NewCache(ctx, CacheConfig[string]{Local: localA, Shared: hooked, Notifier: n, Channel: "inv"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	if err := a.Set(ctx, "k", "from-a", 0); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.Get(ctx, "k"); v != "from-b" {
		t.Fatalf("store holds %q, want from-b", v)
	}
	if v, ok, _ := a.Get(ctx, "k"); !ok || v != "from-b" {
		t.Fatalf("a.Get = %q %v, want the store's from-b", v, ok)
	}
}
