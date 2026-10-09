package graphretriever_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/rag/graphretriever"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/memstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// recordingGraph is a mockGraph that records SearchFacts options and can
// return a search error.
type recordingGraph struct {
	mockGraph
	searchErr error
	got       knowledgetypes.SearchOptions
}

func (g *recordingGraph) SearchFacts(_ context.Context, _ string, opts ...knowledgetypes.SearchOption) (*knowledgetypes.SearchFactsResult, error) {
	for _, o := range opts {
		o(&g.got)
	}
	return &knowledgetypes.SearchFactsResult{Facts: g.facts}, g.searchErr
}

// countingStore counts GetSections calls.
type countingStore struct {
	*memstore.Store
	getSections int
}

func (s *countingStore) GetSections(ctx context.Context, documentUUID string) ([]ragtypes.Section, error) {
	s.getSections++
	return s.Store.GetSections(ctx, documentUUID)
}

func textVariant(uuid, section, text string) ragtypes.ContentVariant {
	return ragtypes.ContentVariant{UUID: uuid, SectionUUID: section, ContentType: ragtypes.ContentText, Text: text}
}

// seedTwoIntros stores a document with two sections both headed "Intro".
func seedTwoIntros(t *testing.T, store ragtypes.Store, docUUID string, meta map[string]string) {
	t.Helper()
	doc := &ragtypes.Document{
		UUID:     docUUID,
		Metadata: meta,
		Sections: []ragtypes.Section{
			{UUID: docUUID + "-s0", DocumentUUID: docUUID, Index: 0, Heading: "Intro",
				Variants: []ragtypes.ContentVariant{textVariant(docUUID+"-v0", docUUID+"-s0", "first intro")}},
			{UUID: docUUID + "-s1", DocumentUUID: docUUID, Index: 1, Heading: "Intro",
				Variants: []ragtypes.ContentVariant{textVariant(docUUID+"-v1", docUUID+"-s1", "second intro")}},
		},
	}
	if err := store.CreateDocument(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
}

func pipelineEpisode(docUUID, section, variant string) knowledgetypes.Episode {
	return knowledgetypes.Episode{
		UUID: "ep-" + variant, Name: "Intro", GroupID: "ns", DocumentID: docUUID,
		Metadata: map[string]string{"section_uuid": section, "variant_uuid": variant, "content_type": "text"},
	}
}

func TestRetrieveResolvesVariantFromEpisodeMetadata(t *testing.T) {
	store := &countingStore{Store: memstore.New()}
	seedTwoIntros(t, store, "doc1", nil)

	graph := &mockGraph{
		facts: []knowledgetypes.Fact{{UUID: "f1", FactText: "fact"}},
		episodes: map[string][]knowledgetypes.Episode{
			"f1": {pipelineEpisode("doc1", "doc1-s1", "doc1-v1")},
		},
	}
	hits, err := graphretriever.New(graph, store).Retrieve(context.Background(), "q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	h := hits[0]
	if h.Variant.UUID != "doc1-v1" || h.Provenance.SectionIndex != 1 || h.Provenance.DocumentUUID != "doc1" {
		t.Errorf("hit = variant %s section %d doc %s, want doc1-v1 section 1 doc1",
			h.Variant.UUID, h.Provenance.SectionIndex, h.Provenance.DocumentUUID)
	}
	if store.getSections != 0 {
		t.Errorf("GetSections calls = %d, want 0", store.getSections)
	}
}

func TestRetrieveOptions(t *testing.T) {
	facts := make([]knowledgetypes.Fact, 0, 40)
	for i := range 40 {
		facts = append(facts, knowledgetypes.Fact{UUID: fmt.Sprintf("f%d", i), FactText: "fact"})
	}

	tests := []struct {
		name      string
		retriever []graphretriever.Option
		opts      *ragtypes.SearchOptions
		facts     []knowledgetypes.Fact
		episodes  map[string][]knowledgetypes.Episode
		searchErr error
		wantLimit int
		wantGroup string
		wantHits  []string // variant UUIDs
		wantErr   error
		wantNoErr bool
	}{
		{
			name:      "limit reaches the graph",
			opts:      &ragtypes.SearchOptions{Limit: 30},
			facts:     facts,
			wantLimit: 30,
			wantNoErr: true,
		},
		{
			name:      "default limit",
			facts:     facts[:3],
			wantLimit: 10,
			wantHits:  []string{"f0", "f1", "f2"},
			wantNoErr: true,
		},
		{
			name:      "group scope reaches the graph",
			retriever: []graphretriever.Option{graphretriever.WithGroupID("tenant-a")},
			facts:     facts[:1],
			wantLimit: 10,
			wantGroup: "tenant-a",
			wantHits:  []string{"f0"},
			wantNoErr: true,
		},
		{
			name:      "image-only search drops text fallback",
			opts:      &ragtypes.SearchOptions{ContentTypes: []ragtypes.ContentType{ragtypes.ContentImage}},
			facts:     facts[:2],
			wantLimit: 40,
			wantHits:  []string{},
			wantNoErr: true,
		},
		{
			name: "metadata filter keeps matching documents only",
			opts: &ragtypes.SearchOptions{MetadataFilters: []ragtypes.MetadataFilter{
				{Key: "tenant", Op: ragtypes.FilterEq, Value: "a"},
			}},
			facts: []knowledgetypes.Fact{{UUID: "fa"}, {UUID: "fb"}, {UUID: "unresolved"}},
			episodes: map[string][]knowledgetypes.Episode{
				"fa": {pipelineEpisode("docA", "docA-s0", "docA-v0")},
				"fb": {pipelineEpisode("docB", "docB-s0", "docB-v0")},
			},
			wantLimit: 40,
			wantHits:  []string{"docA-v0"},
			wantNoErr: true,
		},
		{
			name:      "partial graph error surfaces with hits",
			facts:     facts[:2],
			searchErr: fmt.Errorf("%w: vector search down", knowledgetypes.ErrPartialSearch),
			wantLimit: 10,
			wantHits:  []string{"f0", "f1"},
			wantErr:   ragtypes.ErrPartialSearch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := memstore.New()
			seedTwoIntros(t, store, "docA", map[string]string{"tenant": "a"})
			seedTwoIntros(t, store, "docB", map[string]string{"tenant": "b"})

			graph := &recordingGraph{
				mockGraph: mockGraph{facts: tc.facts, episodes: tc.episodes},
				searchErr: tc.searchErr,
			}
			hits, err := graphretriever.New(graph, store, tc.retriever...).Retrieve(context.Background(), "q", tc.opts)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantNoErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if graph.got.Limit != tc.wantLimit {
				t.Errorf("graph limit = %d, want %d", graph.got.Limit, tc.wantLimit)
			}
			if graph.got.GroupID != tc.wantGroup {
				t.Errorf("graph group = %q, want %q", graph.got.GroupID, tc.wantGroup)
			}
			if tc.wantHits == nil {
				if tc.opts != nil && tc.opts.Limit > 0 && len(hits) != tc.opts.Limit {
					t.Errorf("hits = %d, want %d", len(hits), tc.opts.Limit)
				}
				return
			}
			got := make([]string, len(hits))
			for i, h := range hits {
				got[i] = h.Variant.UUID
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantHits) {
				t.Errorf("hits = %v, want %v", got, tc.wantHits)
			}
		})
	}
}

func TestRetrieveHardGraphErrorFails(t *testing.T) {
	graph := &recordingGraph{searchErr: errors.New("db down")}
	if _, err := graphretriever.New(graph, memstore.New()).Retrieve(context.Background(), "q", nil); err == nil {
		t.Fatal("expected error")
	}
}

// failingProvenanceGraph fails every provenance lookup.
type failingProvenanceGraph struct{ mockGraph }

func (failingProvenanceGraph) GetFactProvenance(context.Context, string) ([]knowledgetypes.Episode, error) {
	return nil, errors.New("provenance down")
}

func TestRetrieveProvenanceErrorIsPartial(t *testing.T) {
	graph := failingProvenanceGraph{mockGraph{facts: []knowledgetypes.Fact{{UUID: "f1", FactText: "fact"}}}}
	hits, err := graphretriever.New(&graph, memstore.New()).Retrieve(context.Background(), "q", nil)
	if !errors.Is(err, ragtypes.ErrPartialSearch) {
		t.Fatalf("err = %v, want ErrPartialSearch", err)
	}
	// A fact whose provenance is unknown may belong to any scope, so it is
	// not shown as bare text.
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want none", hits)
	}
}
