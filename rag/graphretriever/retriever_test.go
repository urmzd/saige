package graphretriever_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/rag/graphretriever"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/memstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

type mockGraph struct {
	facts    []knowledgetypes.Fact
	episodes map[string][]knowledgetypes.Episode // factUUID -> episodes
}

func (m *mockGraph) ApplyOntology(_ context.Context, _ *knowledgetypes.Ontology) error { return nil }
func (m *mockGraph) IngestEpisode(_ context.Context, _ *knowledgetypes.EpisodeInput) (*knowledgetypes.IngestResult, error) {
	return &knowledgetypes.IngestResult{}, nil
}
func (m *mockGraph) GetEntity(_ context.Context, _ string) (*knowledgetypes.Entity, error) {
	return nil, nil
}
func (m *mockGraph) SearchFacts(_ context.Context, _ string, _ ...knowledgetypes.SearchOption) (*knowledgetypes.SearchFactsResult, error) {
	return &knowledgetypes.SearchFactsResult{Facts: m.facts}, nil
}
func (m *mockGraph) GetGraph(_ context.Context, _ int64) (*knowledgetypes.GraphData, error) {
	return nil, nil
}
func (m *mockGraph) GetNode(_ context.Context, _ string, _ int) (*knowledgetypes.NodeDetail, error) {
	return nil, nil
}
func (m *mockGraph) GetFactProvenance(_ context.Context, factUUID string) ([]knowledgetypes.Episode, error) {
	if eps, ok := m.episodes[factUUID]; ok {
		return eps, nil
	}
	return nil, nil
}
func (m *mockGraph) Close(_ context.Context) error { return nil }

func TestGraphRetrieverFallback(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	graph := &mockGraph{
		facts: []knowledgetypes.Fact{
			{UUID: "f1", FactText: "Transformers use self-attention"},
			{UUID: "f2", FactText: "BERT is a language model"},
		},
		episodes: map[string][]knowledgetypes.Episode{},
	}

	r := graphretriever.New(graph, store)
	hits, err := r.Retrieve(ctx, "attention mechanism", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	// Should use fact text as fallback.
	if hits[0].Variant.Text != "Transformers use self-attention" {
		t.Errorf("expected fact text, got %q", hits[0].Variant.Text)
	}

	// Scores should be inverse rank.
	if hits[0].Score <= 0 {
		t.Error("expected positive score")
	}
	if hits[0].Score <= hits[1].Score {
		t.Error("first hit should have higher score than second")
	}
}

func TestGraphRetrieverWithProvenance(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	// Create a document in the store that the graph retriever can resolve to.
	doc := &ragtypes.Document{
		UUID:      "doc1",
		SourceURI: "http://example.com",
		Sections: []ragtypes.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Index:        0,
			Heading:      "Introduction",
			Variants: []ragtypes.ContentVariant{{
				UUID:        "var1",
				SectionUUID: "sec1",
				ContentType: ragtypes.ContentText,
				Text:        "Full variant text about attention.",
			}},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	graph := &mockGraph{
		facts: []knowledgetypes.Fact{
			{UUID: "f1", FactText: "attention is important"},
		},
		episodes: map[string][]knowledgetypes.Episode{
			"f1": {{
				UUID:    "ep1",
				Name:    "Introduction",
				Source:  "http://example.com",
				GroupID: "doc1",
			}},
		},
	}

	r := graphretriever.New(graph, store)
	hits, err := r.Retrieve(ctx, "attention", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}

	// Should have resolved to the store variant.
	if hits[0].Variant.UUID != "var1" {
		t.Errorf("expected resolved variant UUID 'var1', got %q", hits[0].Variant.UUID)
	}
	if hits[0].Variant.Text != "Full variant text about attention." {
		t.Errorf("expected resolved variant text, got %q", hits[0].Variant.Text)
	}
}
