package rag_test

import (
	"context"
	"testing"

	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/memstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

type stubExtractor struct{}

func (e *stubExtractor) Extract(_ context.Context, raw *ragtypes.RawDocument) (*ragtypes.Document, error) {
	return &ragtypes.Document{
		UUID:      "doc-1",
		SourceURI: raw.SourceURI,
		Sections: []ragtypes.Section{{
			UUID: "sec-1", DocumentUUID: "doc-1", Index: 0,
			Variants: []ragtypes.ContentVariant{{
				UUID: "var-1", SectionUUID: "sec-1",
				ContentType: ragtypes.ContentText, Text: string(raw.Data),
			}},
		}},
	}, nil
}

// factGraph is a minimal knowledge graph returning canned facts, with
// episode-deletion recording.
type factGraph struct {
	facts         []knowledgetypes.Fact
	deletedGroups []string
}

func (g *factGraph) ApplyOntology(_ context.Context, _ *knowledgetypes.Ontology) error { return nil }
func (g *factGraph) IngestEpisode(_ context.Context, _ *knowledgetypes.EpisodeInput) (*knowledgetypes.IngestResult, error) {
	return &knowledgetypes.IngestResult{}, nil
}
func (g *factGraph) GetEntity(_ context.Context, _ string) (*knowledgetypes.Entity, error) {
	return nil, nil
}
func (g *factGraph) SearchFacts(_ context.Context, _ string, _ ...knowledgetypes.SearchOption) (*knowledgetypes.SearchFactsResult, error) {
	return &knowledgetypes.SearchFactsResult{Facts: g.facts}, nil
}
func (g *factGraph) GetGraph(_ context.Context, _ int64) (*knowledgetypes.GraphData, error) {
	return nil, nil
}
func (g *factGraph) GetNode(_ context.Context, _ string, _ int) (*knowledgetypes.NodeDetail, error) {
	return nil, nil
}
func (g *factGraph) GetFactProvenance(_ context.Context, _ string) ([]knowledgetypes.Episode, error) {
	return nil, nil
}
func (g *factGraph) Close(_ context.Context) error { return nil }

func (g *factGraph) DeleteEpisodes(_ context.Context, groupID string) error {
	g.deletedGroups = append(g.deletedGroups, groupID)
	return nil
}

// TestWithGraphRegistersGraphRetriever verifies that WithGraph alone (no
// embedders, no explicit retrievers) wires graph-based retrieval into the
// search fusion set.
func TestWithGraphRegistersGraphRetriever(t *testing.T) {
	ctx := context.Background()
	graph := &factGraph{
		facts: []knowledgetypes.Fact{{UUID: "f1", FactText: "saige is a Go SDK"}},
	}

	pipe, err := rag.NewPipeline(
		rag.WithStore(memstore.New()),
		rag.WithContentExtractor(&stubExtractor{}),
		rag.WithGraph(graph),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := pipe.Search(ctx, "saige")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("expected 1 graph-derived hit, got %d", len(result.Hits))
	}
	if result.Hits[0].Variant.Text != "saige is a Go SDK" {
		t.Errorf("expected graph fact text, got %q", result.Hits[0].Variant.Text)
	}
}

// TestWithGraphDeleteRemovesEpisodes verifies the end-to-end wiring: deleting
// an ingested document removes its graph episodes via GraphEpisodeDeleter.
func TestWithGraphDeleteRemovesEpisodes(t *testing.T) {
	ctx := context.Background()
	graph := &factGraph{}

	pipe, err := rag.NewPipeline(
		rag.WithStore(memstore.New()),
		rag.WithContentExtractor(&stubExtractor{}),
		rag.WithGraph(graph),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := pipe.Ingest(ctx, &ragtypes.RawDocument{
		SourceURI: "test://doc",
		Data:      []byte("some content"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := pipe.Delete(ctx, result.DocumentUUID); err != nil {
		t.Fatal(err)
	}
	if len(graph.deletedGroups) != 1 || graph.deletedGroups[0] != result.DocumentUUID {
		t.Errorf("expected DeleteEpisodes(%q), got %v", result.DocumentUUID, graph.deletedGroups)
	}
}
