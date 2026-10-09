package vectorretriever_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

type mockEmbedderRegistry struct {
	result [][]float32
	err    error
}

func (m *mockEmbedderRegistry) Register(_ types.ContentType, _ types.VariantEmbedder) {}
func (m *mockEmbedderRegistry) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

type mockStore struct {
	types.Store
	searchResult  []types.SearchHit
	searchErr     error
	lastEmbedding []float32
}

func (m *mockStore) SearchByEmbedding(_ context.Context, embedding []float32, _ *types.SearchOptions) ([]types.SearchHit, error) {
	m.lastEmbedding = embedding
	return m.searchResult, m.searchErr
}

func TestRetrieveBasic(t *testing.T) {
	embeddings := [][]float32{{0.1, 0.2, 0.3}}
	expectedHits := []types.SearchHit{
		{Variant: types.ContentVariant{UUID: "v1", Text: "result"}, Score: 0.9},
	}

	store := &mockStore{searchResult: expectedHits}
	registry := &mockEmbedderRegistry{result: embeddings}
	r := vectorretriever.New(store, registry)

	hits, err := r.Retrieve(context.Background(), "test query", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Variant.UUID != "v1" {
		t.Errorf("expected variant UUID v1, got %q", hits[0].Variant.UUID)
	}

	// Verify the embedding was passed to the store.
	if len(store.lastEmbedding) != 3 {
		t.Errorf("expected 3-dim embedding passed to store, got %d", len(store.lastEmbedding))
	}
}

func TestRetrieveEmbedError(t *testing.T) {
	registry := &mockEmbedderRegistry{err: fmt.Errorf("embed failure")}
	store := &mockStore{}
	r := vectorretriever.New(store, registry)

	_, err := r.Retrieve(context.Background(), "test", nil)
	if err == nil {
		t.Fatal("expected error from embed failure")
	}
}

func TestRetrieveSearchError(t *testing.T) {
	registry := &mockEmbedderRegistry{result: [][]float32{{0.1}}}
	store := &mockStore{searchErr: fmt.Errorf("search failure")}
	r := vectorretriever.New(store, registry)

	_, err := r.Retrieve(context.Background(), "test", nil)
	if err == nil {
		t.Fatal("expected error from search failure")
	}
}

type purposeRecorder struct {
	purpose types.EmbedPurpose
	result  [][]float32
}

func (p *purposeRecorder) Register(_ types.ContentType, _ types.VariantEmbedder) {}
func (p *purposeRecorder) Embed(ctx context.Context, _ []types.ContentVariant) ([][]float32, error) {
	p.purpose = types.EmbedPurposeFrom(ctx)
	return p.result, nil
}

func TestRetrieveUsesQueryPurpose(t *testing.T) {
	rec := &purposeRecorder{result: [][]float32{{1, 0}}}
	r := vectorretriever.New(&mockStore{}, rec)
	if _, err := r.Retrieve(context.Background(), "q", nil); err != nil {
		t.Fatal(err)
	}
	if rec.purpose != types.PurposeQuery {
		t.Errorf("embed purpose = %q, want %q", rec.purpose, types.PurposeQuery)
	}
}

func TestRetrieveRejectsMalformedQueryEmbedding(t *testing.T) {
	tests := []struct {
		name   string
		result [][]float32
	}{
		{name: "no vectors", result: nil},
		{name: "empty vector", result: [][]float32{{}}},
		{name: "two vectors", result: [][]float32{{1}, {2}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := vectorretriever.New(&mockStore{}, &mockEmbedderRegistry{result: tt.result})
			_, err := r.Retrieve(context.Background(), "q", nil)
			if !errors.Is(err, types.ErrEmbeddingShape) {
				t.Fatalf("expected ErrEmbeddingShape, got %v", err)
			}
		})
	}
}
