// Package embedderregistry provides a concrete implementation of ragtypes.EmbedderRegistry
// that dispatches embedding requests to content-type-specific VariantEmbedders.
package embedderregistry

import (
	"context"
	"fmt"
	"sync"

	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

// Registry implements ragtypes.EmbedderRegistry by dispatching to
// content-type-specific VariantEmbedders with an optional fallback.
type Registry struct {
	mu       sync.RWMutex
	specific map[ragtypes.ContentType]ragtypes.VariantEmbedder
	fallback ragtypes.VariantEmbedder
}

// Option configures a Registry.
type Option func(*Registry)

// WithFallback sets a default embedder used when no type-specific embedder is registered.
func WithFallback(e ragtypes.VariantEmbedder) Option {
	return func(r *Registry) { r.fallback = e }
}

// New creates a new EmbedderRegistry.
func New(opts ...Option) *Registry {
	r := &Registry{
		specific: make(map[ragtypes.ContentType]ragtypes.VariantEmbedder),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewTextOnly creates a registry with a single embedder used for all content types.
func NewTextOnly(embedder ragtypes.VariantEmbedder) *Registry {
	return New(WithFallback(embedder))
}

// Register associates a VariantEmbedder with a specific ContentType.
func (r *Registry) Register(contentType ragtypes.ContentType, embedder ragtypes.VariantEmbedder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specific[contentType] = embedder
}

// Embed dispatches variants to the appropriate embedder by ContentType,
// then reassembles results in the original order.
func (r *Registry) Embed(ctx context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	if len(variants) == 0 {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Group variants by content type, tracking original indices.
	type indexedVariant struct {
		origIdx int
		variant ragtypes.ContentVariant
	}
	groups := make(map[ragtypes.ContentType][]indexedVariant)
	for i, v := range variants {
		groups[v.ContentType] = append(groups[v.ContentType], indexedVariant{origIdx: i, variant: v})
	}

	results := make([][]float32, len(variants))

	for ct, group := range groups {
		embedder, ok := r.specific[ct]
		if !ok {
			embedder = r.fallback
		}
		if embedder == nil {
			return nil, fmt.Errorf("no embedder registered for content type %q and no fallback configured", ct)
		}

		// Build the batch for this embedder.
		batch := make([]ragtypes.ContentVariant, len(group))
		for i, iv := range group {
			batch[i] = iv.variant
		}

		embeddings, err := embedder.Embed(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("embed %q: %w", ct, err)
		}

		// Place results back in original order.
		for i, iv := range group {
			if i < len(embeddings) {
				results[iv.origIdx] = embeddings[i]
			}
		}
	}

	return results, nil
}
