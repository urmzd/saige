package research

import (
	"context"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// stubGraph is a no-op kgtypes.Graph for asserting tool assembly.
type stubGraph struct{}

func (stubGraph) ApplyOntology(context.Context, *kgtypes.Ontology) error { return nil }
func (stubGraph) IngestEpisode(context.Context, *kgtypes.EpisodeInput) (*kgtypes.IngestResult, error) {
	return &kgtypes.IngestResult{}, nil
}
func (stubGraph) GetEntity(context.Context, string) (*kgtypes.Entity, error) { return nil, nil }
func (stubGraph) SearchFacts(context.Context, string, ...kgtypes.SearchOption) (*kgtypes.SearchFactsResult, error) {
	return &kgtypes.SearchFactsResult{}, nil
}
func (stubGraph) GetGraph(context.Context, int64) (*kgtypes.GraphData, error) { return nil, nil }
func (stubGraph) GetNode(context.Context, string, int) (*kgtypes.NodeDetail, error) {
	return nil, nil
}
func (stubGraph) GetFactProvenance(context.Context, string) ([]kgtypes.Episode, error) {
	return nil, nil
}
func (stubGraph) Close(context.Context) error { return nil }

func markerFor(tools []agenttypes.Tool, name string) []agenttypes.Marker {
	for _, tl := range tools {
		if mt, ok := tl.(*agenttypes.MarkedTool); ok && mt.Definition().Name == name {
			return mt.Markers
		}
	}
	return nil
}

func hasTool(tools []agenttypes.Tool, name string) bool {
	for _, tl := range tools {
		if tl.Definition().Name == name {
			return true
		}
	}
	return false
}

func TestStoreKnowledgeGatedByDefault(t *testing.T) {
	tools := NewTools(nil, stubGraph{}, "")

	// Read tools must not be gated.
	for _, name := range []string{"search_knowledge", "read_file", "file_search", "get_knowledge_graph"} {
		if markerFor(tools, name) != nil {
			t.Fatalf("%s should not carry an approval marker", name)
		}
	}

	// store_knowledge mutates the graph; it must carry a human_approval marker.
	markers := markerFor(tools, "store_knowledge")
	if len(markers) == 0 {
		t.Fatal("store_knowledge must carry an approval marker")
	}
	found := false
	for _, m := range markers {
		if m.Kind == "human_approval" {
			found = true
		}
	}
	if !found {
		t.Fatalf("store_knowledge: expected human_approval marker, got %+v", markers)
	}
}

func TestReadOnlyOmitsStoreKnowledge(t *testing.T) {
	tools := NewTools(nil, stubGraph{}, "", ReadOnly())

	if hasTool(tools, "store_knowledge") {
		t.Fatal("read-only mode must omit store_knowledge")
	}
	for _, name := range []string{"search_knowledge", "read_file", "file_search", "get_knowledge_graph"} {
		if !hasTool(tools, name) {
			t.Fatalf("read-only mode must keep read tool %s", name)
		}
	}
}

func TestNoGraphOmitsKnowledgeTools(t *testing.T) {
	tools := NewTools(nil, nil, "")
	for _, name := range []string{"store_knowledge", "search_knowledge", "get_knowledge_graph"} {
		if hasTool(tools, name) {
			t.Fatalf("nil graph should omit %s", name)
		}
	}
	// File tools always present.
	for _, name := range []string{"read_file", "file_search"} {
		if !hasTool(tools, name) {
			t.Fatalf("expected file tool %s", name)
		}
	}
}
