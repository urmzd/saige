// Package embedderregistry provides a concrete implementation of types.EmbedderRegistry
// that dispatches embedding requests to content-type-specific VariantEmbedders.
package embedderregistry

import (
	"context"
	"fmt"
	"sync"

	"github.com/urmzd/saige/rag/types"
)

// Registry implements types.EmbedderRegistry by dispatching to
// content-type-specific VariantEmbedders with an optional fallback.
type Registry struct {
	mu       sync.RWMutex
	specific map[types.ContentType]types.VariantEmbedder
	fallback types.VariantEmbedder
}

// Option configures a Registry.
type Option func(*Registry)

// WithFallback sets a default embedder used when no type-specific embedder is registered.
func WithFallback(e types.VariantEmbedder) Option {
	return func(r *Registry) { r.fallback = e }
}

// New creates a new EmbedderRegistry.
func New(opts ...Option) *Registry {
	r := &Registry{
		specific: make(map[types.ContentType]types.VariantEmbedder),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewTextOnly creates a registry with a single embedder used for all content types.
func NewTextOnly(embedder types.VariantEmbedder) *Registry {
	return New(WithFallback(embedder))
}

// Register associates a VariantEmbedder with a specific ContentType.
func (r *Registry) Register(contentType types.ContentType, embedder types.VariantEmbedder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specific[contentType] = embedder
}

// Embed dispatches variants to the appropriate embedder by ContentType,
// then reassembles results in the original order. It returns an error
// wrapping types.ErrEmbeddingShape when an embedder returns the wrong number
// of vectors, an empty vector, or vectors of mixed dimensions.
func (r *Registry) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	if len(variants) == 0 {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Group variants by content type, tracking original indices.
	type indexedVariant struct {
		origIdx int
		variant types.ContentVariant
	}
	groups := make(map[types.ContentType][]indexedVariant)
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
		batch := make([]types.ContentVariant, len(group))
		for i, iv := range group {
			batch[i] = iv.variant
		}

		embeddings, err := embedder.Embed(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("embed %q: %w", ct, err)
		}
		// Each group comes from one embedder, so its vectors must share a
		// dimension; groups from different embedders may differ.
		if err := types.ValidateEmbeddings(len(batch), embeddings); err != nil {
			return nil, fmt.Errorf("embed %q: %w", ct, err)
		}

		// Place results back in original order.
		for i, iv := range group {
			results[iv.origIdx] = embeddings[i]
		}
	}

	return results, nil
}
