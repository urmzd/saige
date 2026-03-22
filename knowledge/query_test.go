package knowledge

import (
	"testing"

	"github.com/urmzd/saige/knowledge/types"
)

func TestFactsToStrings(t *testing.T) {
	facts := []types.Fact{
		{
			SourceNode: types.Entity{Name: "Alice"},
			TargetNode: types.Entity{Name: "Bob"},
			FactText:   "works with",
		},
		{
			SourceNode: types.Entity{Name: "Go"},
			TargetNode: types.Entity{Name: "Google"},
			FactText:   "created by",
		},
	}

	got := FactsToStrings(facts)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != "Alice -> Bob: works with" {
		t.Errorf("got[0] = %q, want %q", got[0], "Alice -> Bob: works with")
	}
	if got[1] != "Go -> Google: created by" {
		t.Errorf("got[1] = %q, want %q", got[1], "Go -> Google: created by")
	}
}

func TestFactsToStrings_Empty(t *testing.T) {
	got := FactsToStrings(nil)
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestFilterFacts(t *testing.T) {
	facts := []types.Fact{
		{Name: "works_at", FactText: "Alice works at Acme"},
		{Name: "lives_in", FactText: "Alice lives in NYC"},
		{Name: "works_at", FactText: "Bob works at Acme"},
	}

	got := FilterFacts(facts, func(f types.Fact) bool {
		return f.Name == "works_at"
	})

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestFilterFacts_NoneMatch(t *testing.T) {
	facts := []types.Fact{
		{Name: "works_at"},
	}

	got := FilterFacts(facts, func(f types.Fact) bool {
		return f.Name == "lives_in"
	})

	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestFilterByType(t *testing.T) {
	facts := []types.Fact{
		{Name: "works_at", FactText: "Alice works at Acme"},
		{Name: "lives_in", FactText: "Alice lives in NYC"},
		{Name: "works_at", FactText: "Bob works at Acme"},
	}

	got := FilterByType(facts, "works_at")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, f := range got {
		if f.Name != "works_at" {
			t.Errorf("unexpected type %q", f.Name)
		}
	}
}

func TestSubgraph(t *testing.T) {
	detail := &types.NodeDetail{
		Node: types.GraphNode{ID: "1", Name: "Alice", Type: "Person"},
		Neighbors: []types.GraphNode{
			{ID: "2", Name: "Bob", Type: "Person"},
			{ID: "3", Name: "Acme", Type: "Organization"},
		},
		Edges: []types.GraphEdge{
			{ID: "e1", Source: "1", Target: "2", Type: "knows"},
			{ID: "e2", Source: "1", Target: "3", Type: "works_at"},
		},
	}

	got := Subgraph(detail)

	if len(got.Nodes) != 3 {
		t.Errorf("nodes = %d, want 3", len(got.Nodes))
	}
	if got.Nodes[0].ID != "1" {
		t.Errorf("first node should be the center node, got %q", got.Nodes[0].ID)
	}
	if len(got.Edges) != 2 {
		t.Errorf("edges = %d, want 2", len(got.Edges))
	}
}

func TestSubgraph_NoNeighbors(t *testing.T) {
	detail := &types.NodeDetail{
		Node: types.GraphNode{ID: "1", Name: "Lonely"},
	}

	got := Subgraph(detail)
	if len(got.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(got.Nodes))
	}
	if len(got.Edges) != 0 {
		t.Errorf("edges = %d, want 0", len(got.Edges))
	}
}
