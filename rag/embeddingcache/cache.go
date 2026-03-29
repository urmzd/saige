// Package embeddingcache provides an LRU caching decorator for types.VariantEmbedder.
package embeddingcache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/urmzd/saige/rag/types"
)

// Option configures a Cache.
type Option func(*Cache)

// WithMaxSize sets the maximum number of cached embeddings. Default is 10000.
func WithMaxSize(n int) Option {
	return func(c *Cache) {
		if n > 0 {
			c.maxSize = n
		}
	}
}

type entry struct {
	key       string
	embedding []float32
}

// Cache wraps a VariantEmbedder with LRU caching keyed by content hash.
type Cache struct {
	inner   types.VariantEmbedder
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	maxSize int
}

// New creates a caching VariantEmbedder decorator.
func New(inner types.VariantEmbedder, opts ...Option) *Cache {
	c := &Cache{
		inner:   inner,
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		maxSize: 10000,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Embed returns cached embeddings for known variants and delegates to the inner
// embedder for cache misses. Results are stored in the cache with LRU eviction.
func (c *Cache) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	if len(variants) == 0 {
		return nil, nil
	}

	results := make([][]float32, len(variants))
	keys := make([]string, len(variants))

	// Compute cache keys and collect hits.
	var missIndices []int
	c.mu.Lock()
	for i, v := range variants {
		keys[i] = cacheKey(v)
		if elem, ok := c.entries[keys[i]]; ok {
			c.lru.MoveToFront(elem)
			results[i] = elem.Value.(*entry).embedding
		} else {
			missIndices = append(missIndices, i)
		}
	}
	c.mu.Unlock()

	if len(missIndices) == 0 {
		return results, nil
	}

	// Build miss batch and embed.
	missBatch := make([]types.ContentVariant, len(missIndices))
	for i, idx := range missIndices {
		missBatch[i] = variants[idx]
	}

	missEmbeddings, err := c.inner.Embed(ctx, missBatch)
	if err != nil {
		return nil, err
	}

	// Store results and update cache.
	c.mu.Lock()
	for i, idx := range missIndices {
		if i < len(missEmbeddings) {
			results[idx] = missEmbeddings[i]
			c.put(keys[idx], missEmbeddings[i])
		}
	}
	c.mu.Unlock()

	return results, nil
}

// put adds an entry to the cache, evicting the LRU entry if over capacity.
// Must be called with c.mu held.
func (c *Cache) put(key string, embedding []float32) {
	if elem, ok := c.entries[key]; ok {
		c.lru.MoveToFront(elem)
		elem.Value.(*entry).embedding = embedding
		return
	}

	elem := c.lru.PushFront(&entry{key: key, embedding: embedding})
	c.entries[key] = elem

	for c.lru.Len() > c.maxSize {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*entry).key)
	}
}

func cacheKey(v types.ContentVariant) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s:%s", v.ContentType, v.Text)
	return hex.EncodeToString(h.Sum(nil))
}
