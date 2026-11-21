package reranker

import (
	"context"
	"fmt"
	"sort"

	"github.com/urmzd/saige/rag/types"
)

// QueryDocPair represents a query-document pair for cross-encoder scoring.
type QueryDocPair struct {
	Query    string
	Document string
}

// Scorer scores query-document pairs using a cross-encoder model.
type Scorer interface {
	Score(ctx context.Context, pairs []QueryDocPair) ([]float64, error)
}

// CrossEncoderReranker reranks search hits using a cross-encoder scorer.
type CrossEncoderReranker struct {
	scorer Scorer
}

// NewCrossEncoder creates a cross-encoder reranker with the given scorer.
func NewCrossEncoder(scorer Scorer) *CrossEncoderReranker {
	return &CrossEncoderReranker{scorer: scorer}
}

// ReplacesScores reports true: Rerank sets each hit's Score to the
// cross-encoder score (see types.ScoringReranker).
func (r *CrossEncoderReranker) ReplacesScores() bool { return true }

// Rerank scores each hit against the query using the cross-encoder and sorts by score.
func (r *CrossEncoderReranker) Rerank(ctx context.Context, query string, hits []types.SearchHit) ([]types.SearchHit, error) {
	if len(hits) == 0 {
		return hits, nil
	}

	pairs := make([]QueryDocPair, len(hits))
	for i, hit := range hits {
		pairs[i] = QueryDocPair{
			Query:    query,
			Document: hit.Variant.Text,
		}
	}

	scores, err := r.scorer.Score(ctx, pairs)
	if err != nil {
		return nil, fmt.Errorf("cross-encoder score: %w", err)
	}

	if len(scores) != len(hits) {
		return nil, fmt.Errorf("scorer returned %d scores for %d hits", len(scores), len(hits))
	}

	result := make([]types.SearchHit, len(hits))
	copy(result, hits)
	for i := range result {
		result[i].Score = scores[i]
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Score > result[j].Score
	})

	return result, nil
}
