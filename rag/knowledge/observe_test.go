package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/rag/knowledge/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// plainGraph implements only types.Graph.
type plainGraph struct {
	facts []types.Fact
	err   error
}

func (plainGraph) ApplyOntology(context.Context, *types.Ontology) error { return nil }
func (plainGraph) IngestEpisode(context.Context, *types.EpisodeInput) (*types.IngestResult, error) {
	return &types.IngestResult{EntityNodes: make([]types.Entity, 2)}, nil
}
func (plainGraph) GetEntity(context.Context, string) (*types.Entity, error) { return nil, nil }
func (g plainGraph) SearchFacts(context.Context, string, ...types.SearchOption) (*types.SearchFactsResult, error) {
	return &types.SearchFactsResult{Facts: g.facts}, g.err
}
func (plainGraph) GetGraph(context.Context, int64) (*types.GraphData, error)       { return nil, nil }
func (plainGraph) GetNode(context.Context, string, int) (*types.NodeDetail, error) { return nil, nil }
func (plainGraph) GetFactProvenance(context.Context, string) ([]types.Episode, error) {
	return nil, nil
}
func (plainGraph) Close(context.Context) error { return nil }

// deletingGraph adds document deletion with a store that may not support it.
type deletingGraph struct {
	plainGraph
	supported bool
	deleted   []string
}

func (g *deletingGraph) DeleteDocumentEpisodes(_ context.Context, _, documentID string) error {
	g.deleted = append(g.deleted, documentID)
	return nil
}
func (g *deletingGraph) DeleteEpisodes(_ context.Context, groupID string) error {
	g.deleted = append(g.deleted, "group:"+groupID)
	return nil
}
func (g *deletingGraph) SupportsDocumentDeletion() bool { return g.supported }

type spanLog struct {
	ragtypes.NoopObserver
	spans      []string
	retrievals []ragtypes.RetrievalRecord
}

func (s *spanLog) StartSpan(ctx context.Context, name string, attrs ...ragtypes.Attribute) (context.Context, ragtypes.Span) {
	s.spans = append(s.spans, name)
	return s.NoopObserver.StartSpan(ctx, name, attrs...)
}
func (s *spanLog) RecordRetrieval(_ context.Context, rec ragtypes.RetrievalRecord) {
	s.retrievals = append(s.retrievals, rec)
}

func TestObserveForwardsDeletion(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		graph         types.Graph
		wantSupported bool
		wantDeleteErr bool
	}{
		{"plain graph cannot delete", plainGraph{}, false, true},
		{"document deletion supported", &deletingGraph{supported: true}, true, false},
		{"document deletion reported unsupported", &deletingGraph{supported: false}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &spanLog{}
			g := Observe(tt.graph, obs)
			r, ok := g.(ragtypes.GraphDocumentDeletionReporter)
			if !ok || r.SupportsDocumentDeletion() != tt.wantSupported {
				t.Fatalf("SupportsDocumentDeletion = %v, want %v", ok && r.SupportsDocumentDeletion(), tt.wantSupported)
			}
			err := g.(ragtypes.GraphDocumentDeleter).DeleteDocumentEpisodes(ctx, "ns", "doc-1")
			if (err != nil) != tt.wantDeleteErr {
				t.Fatalf("DeleteDocumentEpisodes err = %v", err)
			}
			err = g.(ragtypes.GraphEpisodeDeleter).DeleteEpisodes(ctx, "doc-1")
			if (err != nil) != tt.wantDeleteErr {
				t.Fatalf("DeleteEpisodes err = %v", err)
			}
			if dg, ok := tt.graph.(*deletingGraph); ok && len(dg.deleted) != 2 {
				t.Errorf("deletes forwarded = %v", dg.deleted)
			}
		})
	}
	base := &deletingGraph{}
	if g := Observe(base, nil); g != types.Graph(base) {
		t.Error("a nil observer must return the graph unchanged")
	}
}

func TestObserveSearchAndIngest(t *testing.T) {
	ctx := context.Background()
	partial := errors.Join(types.ErrPartialSearch, errors.New("text arm down"))
	obs := &spanLog{}
	g := Observe(plainGraph{facts: make([]types.Fact, 3), err: partial}, obs)
	res, err := g.SearchFacts(ctx, "q")
	if !errors.Is(err, ragtypes.ErrPartialSearch) || len(res.Facts) != 3 {
		t.Fatalf("SearchFacts = %v, %v; the partial sentinel must be the RAG one", res, err)
	}
	if _, err := g.IngestEpisode(ctx, &types.EpisodeInput{}); err != nil {
		t.Fatal(err)
	}
	if len(obs.spans) != 2 || obs.spans[0] != SpanSearchFacts || obs.spans[1] != SpanIngestEpisode {
		t.Errorf("spans = %v", obs.spans)
	}
	if len(obs.retrievals) != 1 || obs.retrievals[0].Hits != 3 || obs.retrievals[0].Retriever != "knowledge" {
		t.Errorf("retrievals = %+v", obs.retrievals)
	}
}
