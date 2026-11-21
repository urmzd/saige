package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/rag/knowledge/types"
)

// TestGraphReadsScopedToGroup: GetGraphInGroup and GetNodeInGroup never
// return another group's entities or relations.
func TestGraphReadsScopedToGroup(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := must.Get(New(Config{Pool: pool}))

	a1 := mustUpsert(t, store, "a", "Alice", "person", "", testVec(1))
	a2 := mustUpsert(t, store, "a", "Acme", "org", "", testVec(2))
	relA := mustRelate(t, store, "a", a1, a2, "Alice works at Acme")
	b1 := mustUpsert(t, store, "b", "Bob", "person", "", testVec(3))
	b2 := mustUpsert(t, store, "b", "Bobco", "org", "", testVec(4))
	relB := mustRelate(t, store, "b", b1, b2, "Bob works at Bobco")

	g, err := store.GetGraphInGroup(ctx, "a", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 1 || g.Edges[0].ID != relA {
		t.Fatalf("group a edges = %+v, want only %s", g.Edges, relA)
	}
	for _, n := range g.Nodes {
		if n.ID == b1 || n.ID == b2 {
			t.Errorf("group a graph leaked group b node %s", n.Name)
		}
	}

	if _, err := store.GetNodeInGroup(ctx, "a", b1, 2); !errors.Is(err, types.ErrNodeNotFound) {
		t.Errorf("GetNodeInGroup(a, Bob) err = %v, want ErrNodeNotFound", err)
	}
	node, err := store.GetNodeInGroup(ctx, "b", b1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Edges) != 1 || node.Edges[0].ID != relB {
		t.Errorf("group b node edges = %+v, want only %s", node.Edges, relB)
	}

	all, err := store.GetGraph(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Edges) != 2 {
		t.Errorf("unscoped edges = %d, want 2", len(all.Edges))
	}
}

// TestTextSearchMatchesAnyTerm: a query whose terms do not all occur in one
// document still finds the documents that contain some of them.
func TestTextSearchMatchesAnyTerm(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := must.Get(New(Config{Pool: pool}))

	carol := mustUpsert(t, store, "g", "Carol", "person", "", testVec(1))
	dave := mustUpsert(t, store, "g", "Dave", "person", "", testVec(2))
	rel := mustRelate(t, store, "g", carol, dave, "Carol mentors Dave")

	got, err := store.SearchByText(ctx, "who mentors the quarterback", &types.SearchOptions{GroupID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if !factUUIDs(got)[rel] {
		t.Fatalf("search = %v, want %s matched on one of the query terms", factUUIDs(got), rel)
	}
}

// TestEmbeddingSearchTieBreaksByFactText: facts sharing one anchor entity
// are ordered by how well their text matches the query, not only by age.
func TestEmbeddingSearchTieBreaksByFactText(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := must.Get(New(Config{Pool: pool}))

	alice := mustUpsert(t, store, "g", "Alice", "person", "", testVec(1))
	acme := mustUpsert(t, store, "g", "Acme", "org", "", testVec(50))
	tea := mustUpsert(t, store, "g", "Tea", "drink", "", testVec(90))
	works := mustRelate(t, store, "g", alice, acme, "Alice works at Acme")
	// Created later, so recency alone would rank it first.
	likes := mustRelate(t, store, "g", alice, tea, "Alice likes tea")
	if _, err := pool.Exec(ctx, `UPDATE kg_relation SET valid_at = now() + interval '1 day' WHERE uuid = $1`, likes); err != nil {
		t.Fatal(err)
	}

	got, err := store.SearchByEmbedding(ctx, testVec(1), &types.SearchOptions{GroupID: "g", Limit: 1, Query: "where does Alice work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Fact.UUID != works {
		t.Fatalf("top fact = %+v, want %s (text match beats recency)", got, works)
	}
}
