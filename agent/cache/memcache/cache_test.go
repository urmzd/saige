package memcache

import (
	"context"
	"testing"
	"time"
)

func TestCacheGetSetHit(t *testing.T) {
	c := New[string]()
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != "v" {
		t.Fatalf("Get = (%q, %v), want (v, true)", got, found)
	}
}

func TestCacheMiss(t *testing.T) {
	c := New[int]()
	if _, found, _ := c.Get(context.Background(), "absent"); found {
		t.Fatal("expected miss for absent key")
	}
}

func TestCacheDelete(t *testing.T) {
	c := New[int]()
	ctx := context.Background()
	_ = c.Set(ctx, "k", 1, 0)
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := c.Get(ctx, "k"); found {
		t.Fatal("expected miss after delete")
	}
	// Deleting a missing key is not an error.
	if err := c.Delete(ctx, "missing"); err != nil {
		t.Fatalf("delete missing key: %v", err)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := New[string](WithMaxSize[string](2))
	ctx := context.Background()
	_ = c.Set(ctx, "a", "1", 0)
	_ = c.Set(ctx, "b", "2", 0)
	_ = c.Set(ctx, "c", "3", 0) // evicts "a" (oldest)

	if _, found, _ := c.Get(ctx, "a"); found {
		t.Error("expected 'a' to be evicted")
	}
	if _, found, _ := c.Get(ctx, "c"); !found {
		t.Error("expected 'c' to be present")
	}
}

func TestCacheLRUTouchOnGet(t *testing.T) {
	c := New[string](WithMaxSize[string](2))
	ctx := context.Background()
	_ = c.Set(ctx, "a", "1", 0)
	_ = c.Set(ctx, "b", "2", 0)
	_, _, _ = c.Get(ctx, "a") // touch a → b becomes oldest
	_ = c.Set(ctx, "c", "3", 0)

	if _, found, _ := c.Get(ctx, "a"); !found {
		t.Error("expected 'a' to survive (recently used)")
	}
	if _, found, _ := c.Get(ctx, "b"); found {
		t.Error("expected 'b' to be evicted")
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	c := New[string](WithClock[string](clock), WithDefaultTTL[string](time.Minute))
	ctx := context.Background()

	_ = c.Set(ctx, "k", "v", 0) // uses default TTL of 1m
	if _, found, _ := c.Get(ctx, "k"); !found {
		t.Fatal("expected hit before expiry")
	}
	now = now.Add(2 * time.Minute) // advance past TTL
	if _, found, _ := c.Get(ctx, "k"); found {
		t.Fatal("expected miss after TTL expiry")
	}
}

func TestCacheExplicitTTLOverridesDefault(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	c := New[string](WithClock[string](clock), WithDefaultTTL[string](time.Hour))
	ctx := context.Background()

	_ = c.Set(ctx, "k", "v", time.Second) // explicit short TTL
	now = now.Add(2 * time.Second)
	if _, found, _ := c.Get(ctx, "k"); found {
		t.Fatal("expected miss: explicit TTL should override default")
	}
}
