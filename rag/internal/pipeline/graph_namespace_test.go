package pipeline_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/rag/internal/pipeline"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

// docDeletingGraph is a mockGraph that also deletes by document.
type docDeletingGraph struct {
	mockGraph
	inputs     []knowledgetypes.EpisodeInput
	docDeletes [][2]string
}

func (g *docDeletingGraph) IngestEpisode(ctx context.Context, in *knowledgetypes.EpisodeInput) (*knowledgetypes.IngestResult, error) {
	g.inputs = append(g.inputs, *in)
	return g.mockGraph.IngestEpisode(ctx, in)
}

func (g *docDeletingGraph) DeleteDocumentEpisodes(_ context.Context, groupID, documentID string) error {
	g.docDeletes = append(g.docDeletes, [2]string{groupID, documentID})
	return nil
}

func TestPipelineGraphNamespaceLayout(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		docDeleter    bool
		wantGroup     func(docUUID string) string
		wantDocDelete bool
		wantGroupDel  bool
	}{
		{
			name:          "document deleter, default namespace",
			docDeleter:    true,
			wantGroup:     func(string) string { return "" },
			wantDocDelete: true,
			wantGroupDel:  true, // cleans up documents ingested per-document
		},
		{
			name:          "document deleter, tenant namespace",
			namespace:     "tenant-a",
			docDeleter:    true,
			wantGroup:     func(string) string { return "tenant-a" },
			wantDocDelete: true,
			wantGroupDel:  true,
		},
		{
			name:         "group deleter only keeps per-document groups",
			wantGroup:    func(doc string) string { return doc },
			wantGroupDel: true,
		},
		{
			name:         "group deleter only with namespace uses the namespace",
			namespace:    "tenant-a",
			wantGroup:    func(string) string { return "tenant-a" },
			wantGroupDel: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dg := &docDeletingGraph{}
			var graph knowledgetypes.Graph = &dg.mockGraph
			if tc.docDeleter {
				graph = dg
			}
			pipe := pipeline.New(pipeline.Config{
				Store:            memstore.New(),
				ContentExtractor: &simpleExtractor{},
				Graph:            graph,
				GraphNamespace:   tc.namespace,
			})
			result, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "test://ns", Data: []byte("graph text")})
			if err != nil {
				t.Fatal(err)
			}
			doc := result.DocumentUUID

			if len(dg.ingested) == 0 {
				t.Fatal("no episodes ingested")
			}
			for _, g := range dg.ingested {
				if g != tc.wantGroup(doc) {
					t.Errorf("episode GroupID = %q, want %q", g, tc.wantGroup(doc))
				}
			}
			for _, in := range dg.inputs {
				if in.DocumentID != doc {
					t.Errorf("episode DocumentID = %q, want %q", in.DocumentID, doc)
				}
			}

			if err := pipe.Delete(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if got := len(dg.docDeletes) == 1 && dg.docDeletes[0] == [2]string{tc.namespace, doc}; got != tc.wantDocDelete {
				t.Errorf("document deletes = %v, want delete of (%q, %q): %v", dg.docDeletes, tc.namespace, doc, tc.wantDocDelete)
			}
			gotGroupDel := len(dg.deletedGroups) == 1 && dg.deletedGroups[0] == doc
			if gotGroupDel != tc.wantGroupDel {
				t.Errorf("group deletes = %v, want delete of %q: %v", dg.deletedGroups, doc, tc.wantGroupDel)
			}
		})
	}
}
