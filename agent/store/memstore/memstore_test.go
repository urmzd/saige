package memstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/types"
)

func node(id, parent string, depth int, branch types.BranchID) *types.Node {
	return &types.Node{
		ID:        types.NodeID(id),
		ParentID:  types.NodeID(parent),
		Message:   types.NewUserMessage(id),
		State:     types.NodeActive,
		Version:   1,
		Depth:     depth,
		BranchID:  branch,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func TestStoreSaveLoadNode(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	root := node("root", "", 0, "main")
	if err := s.SaveNode(ctx, root); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}

	got, err := s.LoadNode(ctx, "root")
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if got.ID != "root" || got.BranchID != "main" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if _, err := s.LoadNode(ctx, "missing"); err == nil {
		t.Fatal("expected error loading missing node")
	}
}

func TestStoreLoadNodeIsACopy(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	if err := s.SaveNode(ctx, node("n", "", 0, "main")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadNode(ctx, "n")
	got.BranchID = "tampered"

	fresh, _ := s.LoadNode(ctx, "n")
	if fresh.BranchID != "main" {
		t.Fatalf("stored node mutated via returned pointer: %s", fresh.BranchID)
	}
}

func TestStoreChildrenAndPath(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	for _, n := range []*types.Node{
		node("root", "", 0, "main"),
		node("a", "root", 1, "main"),
		node("b", "root", 1, "main"),
		node("c", "a", 2, "main"),
	} {
		if err := s.SaveNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	children, err := s.LoadChildren(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[0].ID != "a" || children[1].ID != "b" {
		t.Fatalf("children order wrong: %+v", children)
	}

	path, err := s.LoadPath(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	want := []types.NodeID{"root", "a", "c"}
	if len(path) != len(want) {
		t.Fatalf("path length: got %d want %d", len(path), len(want))
	}
	for i, id := range want {
		if path[i].ID != id {
			t.Fatalf("path[%d]=%s want %s", i, path[i].ID, id)
		}
	}
}

func TestStoreBranches(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	if err := s.SaveBranch(ctx, "main", "tip1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBranch(ctx, "feature", "tip2"); err != nil {
		t.Fatal(err)
	}

	tip, err := s.LoadBranch(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if tip != "tip1" {
		t.Fatalf("LoadBranch main: got %s", tip)
	}

	all, err := s.ListBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["feature"] != "tip2" {
		t.Fatalf("ListBranches: %+v", all)
	}

	if _, err := s.LoadBranch(ctx, "nope"); err == nil {
		t.Fatal("expected error for missing branch")
	}
}

func TestStoreCheckpoints(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	cp := types.Checkpoint{ID: "cp1", Branch: "main", NodeID: "n1", Name: "snap", CreatedAt: time.Now()}
	if err := s.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadCheckpoint(ctx, "cp1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "snap" || got.NodeID != "n1" {
		t.Fatalf("checkpoint round-trip: %+v", got)
	}
	if _, err := s.LoadCheckpoint(ctx, "missing"); err == nil {
		t.Fatal("expected error for missing checkpoint")
	}
}

func TestStoreLoadTree(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	nodes := []*types.Node{
		node("root", "", 0, "main"),
		node("a", "root", 1, "main"),
		node("b", "a", 2, "main"),
		// A node on a different subtree not reachable from "a"'s root.
		node("orphan", "", 0, "other"),
	}
	for _, n := range nodes {
		if err := s.SaveNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveBranch(ctx, "main", "b"); err != nil {
		t.Fatal(err)
	}

	got, branches, err := s.LoadTree(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	// Only root, a, b are reachable from root — orphan is excluded.
	if len(got) != 3 {
		t.Fatalf("LoadTree returned %d nodes, want 3: %+v", len(got), got)
	}
	if got[0].ID != "root" {
		t.Fatalf("LoadTree not root-first: %s", got[0].ID)
	}
	if branches["main"] != "b" {
		t.Fatalf("LoadTree branches: %+v", branches)
	}

	if _, _, err := s.LoadTree(ctx, "ghost"); err == nil {
		t.Fatal("expected error for unknown root")
	}
}

func TestStoreTxAtomicCommit(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	err := s.Tx(ctx, func(tx types.StoreTx) error {
		if err := tx.SaveNode(ctx, node("root", "", 0, "main")); err != nil {
			return err
		}
		return tx.SaveBranch(ctx, "main", "root")
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if _, err := s.LoadNode(ctx, "root"); err != nil {
		t.Fatalf("committed node missing: %v", err)
	}
	tip, err := s.LoadBranch(ctx, "main")
	if err != nil || tip != "root" {
		t.Fatalf("committed branch wrong: tip=%s err=%v", tip, err)
	}
}

func TestStoreTxRollbackOnError(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	sentinel := errors.New("boom")
	err := s.Tx(ctx, func(tx types.StoreTx) error {
		_ = tx.SaveNode(ctx, node("root", "", 0, "main"))
		_ = tx.SaveBranch(ctx, "main", "root")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error: got %v want %v", err, sentinel)
	}

	// Nothing should have been applied.
	if _, err := s.LoadNode(ctx, "root"); err == nil {
		t.Fatal("rolled-back node was persisted")
	}
	if _, err := s.LoadBranch(ctx, "main"); err == nil {
		t.Fatal("rolled-back branch was persisted")
	}
}
