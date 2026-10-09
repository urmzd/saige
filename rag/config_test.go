package rag_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/rag"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
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

func TestBM25IndexedThroughParentContext(t *testing.T) {
	tests := []struct {
		name string
		opts []rag.Option
	}{
		{name: "bm25 alone", opts: []rag.Option{rag.WithBM25(nil)}},
		{name: "bm25 with parent context", opts: []rag.Option{rag.WithBM25(nil), rag.WithParentContext()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			opts := append([]rag.Option{
				rag.WithStore(memstore.New()),
				rag.WithContentExtractor(&stubExtractor{}),
			}, tt.opts...)
			pipe, err := rag.NewPipeline(opts...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pipe.Ingest(ctx, &ragtypes.RawDocument{Data: []byte("the zebra runs fast")}); err != nil {
				t.Fatal(err)
			}
			result, err := pipe.Search(ctx, "zebra")
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Hits) != 1 {
				t.Errorf("got %d hits for %q, want 1", len(result.Hits), "zebra")
			}
		})
	}
}

func TestBM25ThroughParentContextDeleteAndRebuild(t *testing.T) {
	tests := []struct {
		name string
		opts []rag.Option
	}{
		{name: "bm25 alone", opts: []rag.Option{rag.WithBM25(nil)}},
		{name: "bm25 with parent context", opts: []rag.Option{rag.WithBM25(nil), rag.WithParentContext()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := memstore.New()
			newPipe := func() ragtypes.Pipeline {
				pipe, err := rag.NewPipeline(append([]rag.Option{
					rag.WithStore(store),
					rag.WithContentExtractor(&stubExtractor{}),
				}, tt.opts...)...)
				if err != nil {
					t.Fatal(err)
				}
				return pipe
			}
			search := func(pipe ragtypes.Pipeline) int {
				t.Helper()
				result, err := pipe.Search(ctx, "zebra")
				if err != nil {
					t.Fatal(err)
				}
				return len(result.Hits)
			}

			pipe := newPipe()
			result, err := pipe.Ingest(ctx, &ragtypes.RawDocument{Data: []byte("the zebra runs fast")})
			if err != nil {
				t.Fatal(err)
			}

			// A second pipeline over the same store starts with an empty
			// index until it is rebuilt.
			restarted := newPipe()
			if n := search(restarted); n != 0 {
				t.Fatalf("fresh index returned %d hits", n)
			}
			if err := rag.RebuildIndex(ctx, restarted); err != nil {
				t.Fatal(err)
			}
			if n := search(restarted); n != 1 {
				t.Fatalf("rebuilt index returned %d hits, want 1", n)
			}

			if err := pipe.Delete(ctx, result.DocumentUUID); err != nil {
				t.Fatal(err)
			}
			if n := search(pipe); n != 0 {
				t.Errorf("deleted document still returned %d hits", n)
			}
		})
	}
}

// keywordStore is a memstore whose keyword search is served by the store, as
// pgstore does with pg_search. It records the options each search received.
type keywordStore struct {
	*memstore.Store
	queries []string
	opts    []*ragtypes.SearchOptions
}

func (s *keywordStore) SearchByKeyword(_ context.Context, query string, opts *ragtypes.SearchOptions) ([]ragtypes.SearchHit, error) {
	s.queries = append(s.queries, query)
	s.opts = append(s.opts, opts)
	return []ragtypes.SearchHit{{Variant: ragtypes.ContentVariant{UUID: "store-hit"}, Score: 1}}, nil
}

func TestBM25UsesStoreKeywordSearch(t *testing.T) {
	ctx := context.Background()
	store := &keywordStore{Store: memstore.New()}
	pipe, err := rag.NewPipeline(
		rag.WithStore(store),
		rag.WithContentExtractor(&stubExtractor{}),
		rag.WithBM25(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is ingested: an in-memory index would be empty.
	result, err := pipe.Search(ctx, "zebra", ragtypes.WithScope("tenant"))
	if err != nil {
		t.Fatal(err)
	}
	if len(store.queries) != 1 || store.queries[0] != "zebra" {
		t.Fatalf("store keyword queries = %v, want [zebra]", store.queries)
	}
	if store.opts[0] == nil || store.opts[0].Scope != "tenant" {
		t.Errorf("store keyword search scope = %+v, want tenant", store.opts[0])
	}
	if len(result.Retrievals) != 1 || result.Retrievals[0].Retriever != "bm25" {
		t.Errorf("retrievals = %+v, want one bm25 call", result.Retrievals)
	}
}
