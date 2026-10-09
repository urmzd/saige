// Package vectorretriever implements a Retriever that embeds queries and searches a Store.
package vectorretriever

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/rag/types"
)

// VectorRetriever embeds a query and delegates to store.SearchByEmbedding.
type VectorRetriever struct {
	embedders types.EmbedderRegistry
	store     types.Store
}

// New creates a VectorRetriever with the given store and embedder registry.
func New(store types.Store, embedders types.EmbedderRegistry) *VectorRetriever {
	return &VectorRetriever{store: store, embedders: embedders}
}

// Retrieve embeds the query as a text variant and searches the store. The
// embedding call carries types.PurposeQuery so asymmetric embedders can embed
// the query differently from the indexed documents.
func (r *VectorRetriever) Retrieve(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	queryVariant := types.ContentVariant{
		ContentType: types.ContentText,
		Text:        query,
	}
	embedCtx := types.WithEmbedPurpose(ctx, types.PurposeQuery)
	embeddings, err := r.embedders.Embed(embedCtx, []types.ContentVariant{queryVariant})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if err := types.ValidateEmbeddings(1, embeddings); err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	hits, err := r.store.SearchByEmbedding(ctx, embeddings[0], opts)
	if err != nil {
		return nil, fmt.Errorf("search by embedding: %w", err)
	}
	return hits, nil
}

// Name reports "vector", implementing types.Named.
func (r *VectorRetriever) Name() string { return "vector" }
