// Package memcache is an in-memory LRU implementation of types.Cache with
// optional per-entry TTL. It mirrors the conventions of rag/embeddingcache
// (container/list LRU, mutex-guarded map) and adds time-based expiry.
package memcache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Option configures a Cache.
type Option[V any] func(*Cache[V])

// WithMaxSize sets the maximum number of entries. Default is 1024.
func WithMaxSize[V any](n int) Option[V] {
	return func(c *Cache[V]) {
		if n > 0 {
			c.maxSize = n
		}
	}
}

// WithDefaultTTL sets a fallback TTL applied when Set is called with ttl==0.
// Zero (the default) means entries never expire by time, only by eviction.
func WithDefaultTTL[V any](d time.Duration) Option[V] {
	return func(c *Cache[V]) { c.defaultTTL = d }
}

// WithClock injects a clock for deterministic TTL testing. Default is time.Now.
func WithClock[V any](now func() time.Time) Option[V] {
	return func(c *Cache[V]) {
		if now != nil {
			c.now = now
		}
	}
}

type entry[V any] struct {
	key       string
	value     V
	expiresAt time.Time // zero == no expiry
}

// Cache is an in-memory LRU cache with optional TTL. Safe for concurrent use.
type Cache[V any] struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        *list.List
	maxSize    int
	defaultTTL time.Duration
	now        func() time.Time
}

var _ types.Cache[int] = (*Cache[int])(nil)

// New creates an in-memory LRU cache.
func New[V any](opts ...Option[V]) *Cache[V] {
	c := &Cache[V]{
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		maxSize: 1024,
		now:     time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Get returns the cached value for key, or found=false on a miss or expiry.
func (c *Cache[V]) Get(_ context.Context, key string) (V, bool, error) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return zero, false, nil
	}
	e := elem.Value.(*entry[V])
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
		c.removeElem(elem) // lazy expiry
		return zero, false, nil
	}
	c.lru.MoveToFront(elem)
	return e.value, true, nil
}

// Set stores value under key with the given TTL (0 => default TTL).
func (c *Cache[V]) Set(_ context.Context, key string, value V, ttl time.Duration) error {
	if ttl == 0 {
		ttl = c.defaultTTL
	}
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		c.lru.MoveToFront(elem)
		e := elem.Value.(*entry[V])
		e.value, e.expiresAt = value, exp
		return nil
	}
	elem := c.lru.PushFront(&entry[V]{key: key, value: value, expiresAt: exp})
	c.entries[key] = elem
	for c.lru.Len() > c.maxSize {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.removeElem(oldest)
	}
	return nil
}

// Delete removes key if present. Deleting a missing key is not an error.
func (c *Cache[V]) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		c.removeElem(elem)
	}
	return nil
}

// removeElem deletes an element from both structures. Caller holds c.mu.
func (c *Cache[V]) removeElem(elem *list.Element) {
	c.lru.Remove(elem)
	delete(c.entries, elem.Value.(*entry[V]).key)
}
