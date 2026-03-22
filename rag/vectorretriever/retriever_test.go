package vectorretriever_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
	"github.com/urmzd/graph-agent-dev-kit/rag/vectorretriever"
)

type mockEmbedderRegistry struct {
	result [][]float32
	err    error
}

func (m *mockEmbedderRegistry) Register(_ ragtypes.ContentType, _ ragtypes.VariantEmbedder) {}
func (m *mockEmbedderRegistry) Embed(_ context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

type mockStore struct {
	ragtypes.Store
	searchResult    []ragtypes.SearchHit
	searchErr       error
	lastEmbedding   []float32
}

func (m *mockStore) SearchByEmbedding(_ context.Context, embedding []float32, _ *ragtypes.SearchOptions) ([]ragtypes.SearchHit, error) {
	m.lastEmbedding = embedding
	return m.searchResult, m.searchErr
}

func TestRetrieveBasic(t *testing.T) {
	embeddings := [][]float32{{0.1, 0.2, 0.3}}
	expectedHits := []ragtypes.SearchHit{
		{Variant: ragtypes.ContentVariant{UUID: "v1", Text: "result"}, Score: 0.9},
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
