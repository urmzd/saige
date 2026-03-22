package extraction

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/knowledge/types"
)

// mockExtractor implements types.Extractor for testing.
type mockExtractor struct {
	entities  []types.ExtractedEntity
	relations []types.ExtractedRelation
	err       error
}

func (m *mockExtractor) Extract(_ context.Context, _ string) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	return m.entities, m.relations, m.err
}

// mockEmbedder implements types.Embedder for testing.
type mockEmbedder struct {
	embeddings [][]float32
	err        error
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.embeddings != nil {
		return m.embeddings, nil
	}
	// Generate dummy embeddings
	result := make([][]float32, len(texts))
	for i := range texts {
		result[i] = []float32{0.1, 0.2, 0.3}
	}
	return result, nil
}

func TestPipeline_Process(t *testing.T) {
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
		},
	}
	emb := &mockEmbedder{}

	p := NewPipeline(ext, emb)
	entities, relations, err := p.Process(context.Background(), "Alice works at Acme")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(entities))
	}
	if len(relations) != 1 {
		t.Fatalf("relations = %d, want 1", len(relations))
	}
	// Check embeddings were attached
	for i, e := range entities {
		if len(e.Embedding) == 0 {
			t.Errorf("entity[%d] (%s) has no embedding", i, e.Entity.Name)
		}
	}
}

func TestPipeline_Process_ExtractError(t *testing.T) {
	ext := &mockExtractor{err: errors.New("LLM unavailable")}
	emb := &mockEmbedder{}

	p := NewPipeline(ext, emb)
	_, _, err := p.Process(context.Background(), "test")

	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ext.err) {
		t.Errorf("error = %v, want wrapped %v", err, ext.err)
	}
}

func TestPipeline_Process_NilEmbedder(t *testing.T) {
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
		},
	}

	p := NewPipeline(ext, nil)
	entities, _, err := p.Process(context.Background(), "test")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("entities = %d, want 1", len(entities))
	}
	if len(entities[0].Embedding) != 0 {
		t.Error("expected no embedding with nil embedder")
	}
}

func TestPipeline_Process_EmbedError(t *testing.T) {
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
		},
	}
	emb := &mockEmbedder{err: errors.New("embed failed")}

	p := NewPipeline(ext, emb)
	entities, _, err := p.Process(context.Background(), "test")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still return entities, just without embeddings
	if len(entities) != 1 {
		t.Fatalf("entities = %d, want 1", len(entities))
	}
	if len(entities[0].Embedding) != 0 {
		t.Error("expected no embedding when embedder errors")
	}
}

func TestPipeline_Process_NoEntities(t *testing.T) {
	ext := &mockExtractor{
		entities:  []types.ExtractedEntity{},
		relations: []types.ExtractedRelation{},
	}
	emb := &mockEmbedder{}

	p := NewPipeline(ext, emb)
	entities, relations, err := p.Process(context.Background(), "nothing here")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("entities = %d, want 0", len(entities))
	}
	if len(relations) != 0 {
		t.Errorf("relations = %d, want 0", len(relations))
	}
}
