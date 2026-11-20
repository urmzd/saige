package research

import (
	"context"
	"errors"
	"strings"
	"testing"

	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

// scopedGraph is a stubGraph that also reads per group.
type scopedGraph struct {
	stubGraph
	unscopedCalls int
	gotGroup      string
	gotLimit      int64
}

func (g *scopedGraph) GetGraph(context.Context, int64) (*kgtypes.GraphData, error) {
	g.unscopedCalls++
	return &kgtypes.GraphData{}, nil
}

func (g *scopedGraph) GetGraphInGroup(_ context.Context, groupID string, limit int64) (*kgtypes.GraphData, error) {
	g.gotGroup, g.gotLimit = groupID, limit
	return &kgtypes.GraphData{
		Nodes: []kgtypes.GraphNode{{ID: "a", Name: "Alice"}, {ID: "b", Name: "Acme"}},
		Edges: []kgtypes.GraphEdge{{ID: "r", Source: "a", Target: "b", Type: "works_at", Fact: "Alice works at Acme"}},
	}, nil
}

func (g *scopedGraph) GetNodeInGroup(context.Context, string, string, int) (*kgtypes.NodeDetail, error) {
	return nil, kgtypes.ErrNodeNotFound
}

func TestGetGraphToolScopedToGroup(t *testing.T) {
	g := &scopedGraph{}
	out, err := NewGetGraphTool(g).WithGroupID("tenant-a").Execute(context.Background(), map[string]any{"limit": float64(7)})
	if err != nil {
		t.Fatal(err)
	}
	if g.unscopedCalls != 0 {
		t.Fatalf("unscoped GetGraph called %d times, want 0 for a group-bound tool", g.unscopedCalls)
	}
	if g.gotGroup != "tenant-a" || g.gotLimit != 7 {
		t.Errorf("GetGraphInGroup(%q, %d), want (tenant-a, 7)", g.gotGroup, g.gotLimit)
	}
	if !strings.Contains(out, "Alice") {
		t.Errorf("output %q missing scoped graph", out)
	}
}

func TestGetGraphToolFailsClosedWithoutGroupSupport(t *testing.T) {
	_, err := NewGetGraphTool(stubGraph{}).WithGroupID("tenant-a").Execute(context.Background(), map[string]any{})
	if !errors.Is(err, kgtypes.ErrGroupScopeUnsupported) {
		t.Fatalf("err = %v, want ErrGroupScopeUnsupported instead of an unscoped read", err)
	}
}

func TestGetGraphToolWithoutGroupReadsAll(t *testing.T) {
	g := &scopedGraph{}
	if _, err := NewGetGraphTool(g).Execute(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if g.unscopedCalls != 1 || g.gotGroup != "" {
		t.Errorf("unscoped calls = %d, scoped group = %q; want 1 unscoped call", g.unscopedCalls, g.gotGroup)
	}
}
