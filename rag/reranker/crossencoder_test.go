package reranker_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/rag/reranker"
	"github.com/urmzd/saige/rag/types"
)

type mockScorer struct{}

func (m *mockScorer) Score(_ context.Context, pairs []reranker.QueryDocPair) ([]float64, error) {
	scores := make([]float64, len(pairs))
	for i, pair := range pairs {
		// Score based on document length (longer = higher score, for testing).
		scores[i] = float64(len(pair.Document)) / 100.0
	}
	return scores, nil
}

func TestCrossEncoderReranker(t *testing.T) {
	hits := []types.SearchHit{
		{Variant: types.ContentVariant{UUID: "short", Text: "hi"}, Score: 1.0},
		{Variant: types.ContentVariant{UUID: "medium", Text: "hello world"}, Score: 0.5},
		{Variant: types.ContentVariant{UUID: "long", Text: "this is a much longer document with many words"}, Score: 0.1},
	}

	r := reranker.NewCrossEncoder(&mockScorer{})
	result, err := r.Rerank(context.Background(), "test query", hits)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}

	// Longest document should be first (highest score from our mock scorer).
	if result[0].Variant.UUID != "long" {
		t.Errorf("expected 'long' first, got %q", result[0].Variant.UUID)
	}
	if result[1].Variant.UUID != "medium" {
		t.Errorf("expected 'medium' second, got %q", result[1].Variant.UUID)
	}
	if result[2].Variant.UUID != "short" {
		t.Errorf("expected 'short' third, got %q", result[2].Variant.UUID)
	}
}

func TestCrossEncoderEmpty(t *testing.T) {
	r := reranker.NewCrossEncoder(&mockScorer{})
	result, err := r.Rerank(context.Background(), "q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result))
	}
}
