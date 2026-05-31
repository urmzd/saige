package tool_test

import (
	"context"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/knowledge/tool"
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
func (stubGraph) GetGraph(context.Context, int64) (*kgtypes.GraphData, error)       { return nil, nil }
func (stubGraph) GetNode(context.Context, string, int) (*kgtypes.NodeDetail, error) { return nil, nil }
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

func TestDefaultGatesMutatingTool(t *testing.T) {
	tools := tool.NewTools(stubGraph{})

	// kg_search is a read tool, must not be gated.
	if markerFor(tools, "kg_search") != nil {
		t.Fatal("kg_search should not carry an approval marker")
	}
	if !hasTool(tools, "kg_search") {
		t.Fatal("kg_search must be present")
	}

	// kg_ingest mutates the graph, must carry a human_approval marker.
	markers := markerFor(tools, "kg_ingest")
	if len(markers) == 0 {
		t.Fatal("kg_ingest must carry an approval marker")
	}
	found := false
	for _, m := range markers {
		if m.Kind == "human_approval" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kg_ingest: expected human_approval marker, got %+v", markers)
	}
}

func TestReadOnlyOmitsIngest(t *testing.T) {
	tools := tool.NewTools(stubGraph{}, tool.ReadOnly())

	if hasTool(tools, "kg_ingest") {
		t.Fatal("read-only mode must omit kg_ingest")
	}
	if !hasTool(tools, "kg_search") {
		t.Fatal("read-only mode must keep kg_search")
	}
}

// TestMarkedToolDelegates verifies the gate is transparent to execution: the
// wrapped tool still runs through the MarkedTool wrapper.
func TestMarkedToolDelegates(t *testing.T) {
	tools := tool.NewTools(stubGraph{})
	var ingest agenttypes.Tool
	for _, tl := range tools {
		if tl.Definition().Name == "kg_ingest" {
			ingest = tl
		}
	}
	if ingest == nil {
		t.Fatal("kg_ingest not found")
	}
	out, err := ingest.Execute(context.Background(), map[string]any{"name": "n", "body": "b"})
	if err != nil {
		t.Fatalf("execute through marker: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}
}
