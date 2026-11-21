package reranker

import (
	"context"

	"github.com/urmzd/saige/rag/types"
)

// Chain runs rerankers in order, each over the previous one's output. The
// usual chain is a cross-encoder for relevance followed by MMR for
// diversity.
type Chain []types.Reranker

// Rerank implements types.Reranker. It stops at the first error.
func (c Chain) Rerank(ctx context.Context, query string, hits []types.SearchHit) ([]types.SearchHit, error) {
	var err error
	for _, r := range c {
		if hits, err = r.Rerank(ctx, query, hits); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

// ReplacesScores reports whether the chain's final scores come from a
// scoring stage and no later stage reorders them away from score order:
// true when the last stage is a scoring reranker. With MMR last, the order
// is MMR's and the pipeline must not re-sort by score.
func (c Chain) ReplacesScores() bool {
	if len(c) == 0 {
		return false
	}
	s, ok := c[len(c)-1].(types.ScoringReranker)
	return ok && s.ReplacesScores()
}
