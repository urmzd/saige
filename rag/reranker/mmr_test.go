package reranker_test

import (
	"context"
	"slices"
	"testing"

	"github.com/urmzd/saige/rag/reranker"
	"github.com/urmzd/saige/rag/types"
)

func TestMMRRerankerDiversity(t *testing.T) {
	// Create hits with synthetic embeddings.
	// Hit A and B are very similar (same embedding), Hit C is different.
	hits := []types.SearchHit{
		{
			Variant:    types.ContentVariant{UUID: "a", Text: "similar 1", Embedding: []float32{1, 0, 0, 0}},
			Score:      1.0,
			Provenance: types.Provenance{DocumentUUID: "d1"},
		},
		{
			Variant:    types.ContentVariant{UUID: "b", Text: "similar 2", Embedding: []float32{1, 0, 0, 0}},
			Score:      0.9,
			Provenance: types.Provenance{DocumentUUID: "d1"},
		},
		{
			Variant:    types.ContentVariant{UUID: "c", Text: "different", Embedding: []float32{0, 1, 0, 0}},
			Score:      0.8,
			Provenance: types.Provenance{DocumentUUID: "d2"},
		},
	}

	r := reranker.NewMMR(0.5) // Balance relevance and diversity.
	result, err := r.Rerank(context.Background(), "query", hits)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}

	// First should be "a" (highest relevance).
	if result[0].Variant.UUID != "a" {
		t.Errorf("expected 'a' first, got %q", result[0].Variant.UUID)
	}

	// Second should be "c" (diverse from "a"), not "b" (similar to "a").
	if result[1].Variant.UUID != "c" {
		t.Errorf("expected 'c' second for diversity, got %q", result[1].Variant.UUID)
	}
}

func TestMMRSingleHit(t *testing.T) {
	hits := []types.SearchHit{
		{Variant: types.ContentVariant{UUID: "a"}, Score: 1.0},
	}

	r := reranker.NewMMR(0.7)
	result, err := r.Rerank(context.Background(), "q", hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
}

func TestMMRRerankerScales(t *testing.T) {
	hit := func(uuid string, score float64, emb []float32) types.SearchHit {
		return types.SearchHit{Variant: types.ContentVariant{UUID: uuid, Embedding: emb}, Score: score}
	}
	tests := []struct {
		name   string
		lambda float64
		hits   []types.SearchHit
		want   []string
	}{
		{
			// RRF-scale scores must still let relevance dominate at a high
			// lambda: a and b are identical, c is orthogonal and weak.
			name:   "rrf scale relevance",
			lambda: 0.9,
			hits: []types.SearchHit{
				hit("a", 0.032, []float32{1, 0}),
				hit("b", 0.031, []float32{1, 0}),
				hit("c", 0.016, []float32{0, 1}),
			},
			want: []string{"a", "b", "c"},
		},
		{
			// A hit without an embedding must not be favored as diverse.
			name:   "missing embedding is not diverse",
			lambda: 0.5,
			hits: []types.SearchHit{
				hit("a", 1.0, []float32{1, 0}),
				hit("noemb", 0.9, nil),
				hit("c", 0.8, []float32{0, 1}),
			},
			want: []string{"a", "c", "noemb"},
		},
		{
			name:   "equal scores keep input order without embeddings",
			lambda: 0.7,
			hits:   []types.SearchHit{hit("a", 0.5, nil), hit("b", 0.5, nil)},
			want:   []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := slices.Clone(tt.hits)
			result, err := reranker.NewMMR(tt.lambda).Rerank(context.Background(), "q", input)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range result {
				got = append(got, h.Variant.UUID)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("order = %v, want %v", got, tt.want)
			}
			// Scores keep their scale, and the input is untouched.
			original := make(map[string]float64)
			for _, h := range tt.hits {
				original[h.Variant.UUID] = h.Score
			}
			for _, h := range result {
				if h.Score != original[h.Variant.UUID] {
					t.Errorf("%s score = %v, want %v", h.Variant.UUID, h.Score, original[h.Variant.UUID])
				}
			}
			for i := range input {
				if input[i].Variant.UUID != tt.hits[i].Variant.UUID || input[i].Score != tt.hits[i].Score {
					t.Errorf("input mutated at %d: %+v", i, input[i])
				}
			}
		})
	}
}
