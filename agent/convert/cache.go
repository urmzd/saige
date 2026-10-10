package convert

import (
	"container/list"
	"context"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// DefaultCacheEntries bounds a MemoryCache made with a non-positive size.
const DefaultCacheEntries = 1024

// MemoryCache is an in-process types.ConversionCache that keeps the most
// recently used entries.
type MemoryCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type cacheItem struct {
	key   string
	entry types.ConversionEntry
}

var _ types.ConversionCache = (*MemoryCache)(nil)

// NewMemoryCache returns a cache of at most entries conversions.
func NewMemoryCache(entries int) *MemoryCache {
	if entries <= 0 {
		entries = DefaultCacheEntries
	}
	return &MemoryCache{max: entries, order: list.New(), items: map[string]*list.Element{}}
}

// Get implements types.ConversionCache.
func (c *MemoryCache) Get(_ context.Context, key string) (types.ConversionEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return types.ConversionEntry{}, false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*cacheItem).entry
	return types.ConversionEntry{Parts: types.CloneParts(e.Parts), Via: e.Via}, true
}

// Put implements types.ConversionCache.
func (c *MemoryCache) Put(_ context.Context, key string, e types.ConversionEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e = types.ConversionEntry{Parts: types.CloneParts(e.Parts), Via: e.Via}
	if el, ok := c.items[key]; ok {
		el.Value.(*cacheItem).entry = e
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&cacheItem{key: key, entry: e})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*cacheItem).key)
	}
}

// Len returns the number of entries.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
