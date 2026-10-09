// Package reranker provides reranking strategies for search results.
package reranker

import (
	"context"
	"math"
	"slices"

	"github.com/urmzd/saige/rag/types"
)

// MMRConfig holds MMR reranker parameters.
type MMRConfig struct {
	Lambda float64 // Higher = more relevance vs diversity. Default 0.7.
}

// MMRReranker implements maximal marginal relevance reranking for diversity.
type MMRReranker struct {
	lambda float64
}

// NewMMR creates an MMR diversity reranker. If lambda is 0, defaults to 0.7.
func NewMMR(lambda float64) *MMRReranker {
	if lambda == 0 {
		lambda = 0.7
	}
	return &MMRReranker{lambda: lambda}
}

// Rerank selects hits greedily to maximize relevance while minimizing redundancy.
//
// Relevance is each hit's Score min-max normalized to [0,1] across the input,
// so it is on the same scale as the cosine similarity used for redundancy
// whatever scale the scores came from (for example small RRF values). A hit
// without an embedding, or whose embedding cannot be compared, counts as fully
// redundant with any already selected hit, so it never jumps ahead of a
// comparable hit only because its similarity is unknown.
//
// Rerank returns a new slice in MMR order. The input slice is not modified
// and every hit keeps its original Score.
func (r *MMRReranker) Rerank(_ context.Context, _ string, hits []types.SearchHit) ([]types.SearchHit, error) {
	if len(hits) <= 1 {
		return slices.Clone(hits), nil
	}

	relevance := normalizedScores(hits)
	selected := make([]int, 0, len(hits))
	remaining := make([]int, len(hits))
	for i := range remaining {
		remaining[i] = i
	}

	for len(remaining) > 0 {
		bestPos := -1
		bestMMR := math.Inf(-1)

		for pos, ri := range remaining {
			maxSim := 0.0
			for si, s := range selected {
				sim, ok := similarity(hits[ri].Variant.Embedding, hits[s].Variant.Embedding)
				if !ok {
					sim = 1
				}
				if si == 0 || sim > maxSim {
					maxSim = sim
				}
			}

			mmr := r.lambda*relevance[ri] - (1-r.lambda)*maxSim
			if mmr > bestMMR {
				bestMMR = mmr
				bestPos = pos
			}
		}

		selected = append(selected, remaining[bestPos])
		remaining = append(remaining[:bestPos], remaining[bestPos+1:]...)
	}

	result := make([]types.SearchHit, len(selected))
	for i, idx := range selected {
		result[i] = hits[idx]
	}
	return result, nil
}

// normalizedScores min-max scales hit scores to [0,1]. Equal scores all map
// to 1.
func normalizedScores(hits []types.SearchHit) []float64 {
	lo, hi := hits[0].Score, hits[0].Score
	for _, h := range hits[1:] {
		lo = min(lo, h.Score)
		hi = max(hi, h.Score)
	}
	out := make([]float64, len(hits))
	for i, h := range hits {
		if hi == lo {
			out[i] = 1
			continue
		}
		out[i] = (h.Score - lo) / (hi - lo)
	}
	return out
}

// similarity returns the cosine similarity of a and b, and false when the
// vectors cannot be compared (missing, of different lengths, or zero).
func similarity(a, b []float32) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var normA, normB float64
	for i := range a {
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0, false
	}
	return cosineSimilarity(a, b), true
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
