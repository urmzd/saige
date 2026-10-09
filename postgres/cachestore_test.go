package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/provider/cache"
	"github.com/urmzd/saige/agent/toolcache"
	"github.com/urmzd/saige/agent/types"
)

func TestCacheStoreTTL(t *testing.T) {
	pool := notifyDatabase(t)
	ctx := context.Background()
	s := NewCacheStore(pool, CacheStoreOptions{})

	if _, ok, err := s.Get(ctx, "missing"); ok || err != nil {
		t.Fatalf("Get(missing) = %v, %v", ok, err)
	}
	if err := s.Set(ctx, "forever", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "short", []byte("v"), 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "empty", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"forever", "short", "empty"} {
		if _, ok, err := s.Get(ctx, k); !ok || err != nil {
			t.Fatalf("Get(%s) = %v, %v", k, ok, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok, _ := s.Get(ctx, "short"); ok {
		t.Fatal("expired entry returned")
	}
	if v, ok, _ := s.Get(ctx, "forever"); !ok || string(v) != "v" {
		t.Fatal("entry without TTL expired")
	}

	// Overwrite replaces value and expiry.
	if err := s.Set(ctx, "short", []byte("v2"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.Get(ctx, "short"); !ok || string(v) != "v2" {
		t.Fatalf("overwrite = %q %v", v, ok)
	}

	if err := s.Set(ctx, "gone", []byte("v"), -time.Second); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Sweep(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("Sweep = %d, %v", removed, err)
	}
	if err := s.Delete(ctx, "forever"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "forever"); ok {
		t.Fatal("deleted entry returned")
	}
	if err := s.Delete(ctx, "forever"); err != nil {
		t.Fatal("deleting a missing key failed:", err)
	}

	def := NewCacheStore(pool, CacheStoreOptions{DefaultTTL: 100 * time.Millisecond})
	if err := def.Set(ctx, "default", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, ok, _ := def.Get(ctx, "default"); ok {
		t.Fatal("DefaultTTL not applied")
	}
}

func TestCacheStoreSweeper(t *testing.T) {
	pool := notifyDatabase(t)
	ctx := context.Background()
	s := NewCacheStore(pool, CacheStoreOptions{})
	if err := s.Set(ctx, "k", []byte("v"), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	stop := s.StartSweeper(ctx, 20*time.Millisecond, nil)
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM saige_cache`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not remove the expired row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
}

// Two processes share one CacheStore and keep per-process levels coherent
// through the Postgres notifier, for both the response cache and the tool
// cache codecs.
func TestCacheStoreInvalidation(t *testing.T) {
	pool := notifyDatabase(t)
	ctx := context.Background()
	store := NewCacheStore(pool, CacheStoreOptions{})

	n1 := NewNotifier(pool, NotifierOptions{})
	defer n1.Close()
	n2 := NewNotifier(pool, NotifierOptions{})
	defer n2.Close()

	local1, local2 := memcache.New[toolcache.Entry](), memcache.New[toolcache.Entry]()
	p1, err := notify.NewCache(ctx, notify.CacheConfig[toolcache.Entry]{
		Local: local1, Shared: toolcache.BytesCache(store), Notifier: n1, Channel: "saige.cache.tool",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := notify.NewCache(ctx, notify.CacheConfig[toolcache.Entry]{
		Local: local2, Shared: toolcache.BytesCache(store), Notifier: n2, Channel: "saige.cache.tool",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	entry := func(text string) toolcache.Entry {
		now := time.Now().UTC().Truncate(time.Millisecond)
		return toolcache.Entry{Result: types.ToolResult{Text: text}, StoredAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	if err := p1.Set(ctx, "k", entry("v1"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := p2.Get(ctx, "k"); !ok || err != nil || e.Result.Text != "v1" {
		t.Fatalf("p2.Get = %+v %v %v", e, ok, err)
	}
	if err := p1.Set(ctx, "k", entry("v2"), time.Hour); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { _, ok, _ := local2.Get(ctx, "k"); return !ok })
	if e, _, _ := p2.Get(ctx, "k"); e.Result.Text != "v2" {
		t.Fatalf("p2 read stale %q", e.Result.Text)
	}
	if err := p2.Invalidate(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { _, ok, _ := local1.Get(ctx, "k"); return !ok })
	if _, ok, _ := p1.Get(ctx, "k"); ok {
		t.Fatal("invalidated entry still readable")
	}

	// The response cache adapter round-trips through the same store.
	responses := cache.BytesCache(store)
	cr := cache.CachedResponse{
		Deltas: []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "hi"}, types.TextEndDelta{}},
		Usage:  types.UsageDelta{PromptTokens: 3, CompletionTokens: 1},
	}
	if err := responses.Set(ctx, "r", cr, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, ok, err := responses.Get(ctx, "r")
	if err != nil || !ok || len(got.Deltas) != 3 || got.Usage.PromptTokens != 3 {
		t.Fatalf("response round trip = %+v %v %v", got, ok, err)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
