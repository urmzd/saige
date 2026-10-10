// Package storetest is a conformance suite for agent/types.Store
// implementations. Call RunConformance from each implementation's tests so
// every Store makes the same promises about versions, ordering, scoping,
// transactions and the optional active-branch and deletion capabilities.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// NewStore returns a Store scoped to conversationID. Within one call of a
// subtest, stores for different conversation IDs must share one backend, so
// the suite can check that scoping keeps them apart.
type NewStore func(t *testing.T, conversationID string) types.Store

// ConversationDeleter is implemented by stores that can delete everything
// they hold for their conversation.
type ConversationDeleter = types.ConversationDeleter

// RunConformance runs the suite. Node, branch and conversation IDs are unique
// per subtest, so a shared database needs no cleanup between runs.
func RunConformance(t *testing.T, newStore NewStore) {
	t.Helper()
	for _, tc := range []struct {
		name string
		run  func(*testing.T, NewStore)
	}{
		{"SaveLoadNode", testSaveLoadNode},
		{"StaleVersionConflicts", testStaleVersion},
		{"SameVersionIsNoop", testSameVersion},
		{"NewerVersionUpdates", testNewerVersion},
		{"ChildOrder", testChildOrder},
		{"LoadPathAndTree", testLoadPathAndTree},
		{"Branches", testBranches},
		{"Checkpoints", testCheckpoints},
		{"TxRollsBackOnError", testTxRollback},
		{"TxStaleVersionConflicts", testTxStaleVersion},
		{"CrossConversationNodeNotFound", testCrossConversation},
		{"ActiveBranch", testActiveBranch},
		{"DeleteConversation", testDeleteConversation},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newStore) })
	}
}

func newID(prefix string) string { return prefix + "-" + types.NewID()[:12] }

func node(id, parent types.NodeID, depth int, version uint64, text string) *types.Node {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &types.Node{
		ID:        id,
		ParentID:  parent,
		Message:   types.UserMsg(types.Text(text)),
		Version:   version,
		Depth:     depth,
		BranchID:  "main",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func text(t *testing.T, n *types.Node) string {
	t.Helper()
	msg, ok := n.Message.(types.UserMessage)
	if !ok || len(msg.Parts) == 0 {
		t.Fatalf("node %s message = %#v, want a user message", n.ID, n.Message)
	}
	tc, ok := msg.Parts[0].(types.TextPart)
	if !ok {
		t.Fatalf("node %s content = %#v, want text", n.ID, msg.Parts[0])
	}
	return tc.Text
}

func save(t *testing.T, s types.Store, nodes ...*types.Node) {
	t.Helper()
	for _, n := range nodes {
		if err := s.SaveNode(context.Background(), n); err != nil {
			t.Fatalf("SaveNode %s: %v", n.ID, err)
		}
	}
}

func testSaveLoadNode(t *testing.T, newStore NewStore) {
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	save(t, s, node(root, "", 0, 1, "hello"))
	got, err := s.LoadNode(context.Background(), root)
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if got.Version != 1 || text(t, got) != "hello" {
		t.Errorf("loaded %+v", got)
	}
	if _, err := s.LoadNode(context.Background(), types.NodeID(newID("missing"))); err == nil {
		t.Error("LoadNode of a missing node returned no error")
	}
}

func testStaleVersion(t *testing.T, newStore NewStore) {
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	save(t, s, node(root, "", 0, 1, "v1"), node(root, "", 0, 3, "v3"))
	err := s.SaveNode(context.Background(), node(root, "", 0, 2, "v2"))
	if !errors.Is(err, tree.ErrVersionConflict) {
		t.Fatalf("stale SaveNode error = %v, want ErrVersionConflict", err)
	}
	got, _ := s.LoadNode(context.Background(), root)
	if got.Version != 3 || text(t, got) != "v3" {
		t.Errorf("stale write changed the node: %+v", got)
	}
}

func testSameVersion(t *testing.T, newStore NewStore) {
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	save(t, s, node(root, "", 0, 1, "first"), node(root, "", 0, 1, "second"))
	got, _ := s.LoadNode(context.Background(), root)
	if text(t, got) != "first" {
		t.Errorf("same-version write replaced the node: %q", text(t, got))
	}
}

func testNewerVersion(t *testing.T, newStore NewStore) {
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	save(t, s, node(root, "", 0, 1, "v1"))
	archived := node(root, "", 0, 2, "v2")
	archived.State = types.NodeArchived
	archived.ArchivedBy = "tester"
	save(t, s, archived)
	got, _ := s.LoadNode(context.Background(), root)
	if got.Version != 2 || got.State != types.NodeArchived || got.ArchivedBy != "tester" || text(t, got) != "v2" {
		t.Errorf("newer write not applied: %+v", got)
	}
}

func testChildOrder(t *testing.T, newStore NewStore) {
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	save(t, s, node(root, "", 0, 1, "root"))
	var want []types.NodeID
	for i := range 4 {
		id := types.NodeID(newID("child"))
		want = append(want, id)
		save(t, s, node(id, root, 1, 1, string(rune('a'+i))))
	}
	// Rewriting an existing child must not move it.
	save(t, s, node(want[0], root, 1, 2, "a2"))
	kids, err := s.LoadChildren(context.Background(), root)
	if err != nil {
		t.Fatalf("LoadChildren: %v", err)
	}
	if len(kids) != len(want) {
		t.Fatalf("children = %d, want %d", len(kids), len(want))
	}
	for i, k := range kids {
		if k.ID != want[i] {
			t.Errorf("child %d = %s, want %s", i, k.ID, want[i])
		}
	}
}

func testLoadPathAndTree(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	a := types.NodeID(newID("a"))
	b := types.NodeID(newID("b"))
	c := types.NodeID(newID("c"))
	save(t, s, node(root, "", 0, 1, "root"), node(a, root, 1, 1, "a"), node(b, a, 2, 1, "b"), node(c, root, 1, 1, "c"))
	if err := s.SaveBranch(ctx, "main", b); err != nil {
		t.Fatalf("SaveBranch: %v", err)
	}
	path, err := s.LoadPath(ctx, b)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	if len(path) != 3 || path[0].ID != root || path[1].ID != a || path[2].ID != b {
		t.Errorf("path = %v", ids(path))
	}
	nodes, branches, err := s.LoadTree(ctx, root)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(nodes) != 4 || nodes[0].ID != root {
		t.Errorf("tree nodes = %v, want 4 nodes root first", ids(nodes))
	}
	if branches["main"] != b {
		t.Errorf("tree branches = %v", branches)
	}
}

func ids(nodes []*types.Node) []types.NodeID {
	out := make([]types.NodeID, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func testBranches(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	s := newStore(t, newID("conv"))
	if err := s.SaveBranch(ctx, "main", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBranch(ctx, "main", "n2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBranch(ctx, "side", "n3"); err != nil {
		t.Fatal(err)
	}
	tip, err := s.LoadBranch(ctx, "main")
	if err != nil || tip != "n2" {
		t.Errorf("LoadBranch = %s, %v; want n2", tip, err)
	}
	all, err := s.ListBranches(ctx)
	if err != nil || len(all) != 2 || all["side"] != "n3" {
		t.Errorf("ListBranches = %v, %v", all, err)
	}
	if _, err := s.LoadBranch(ctx, "missing"); err == nil {
		t.Error("LoadBranch of a missing branch returned no error")
	}
}

func testCheckpoints(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	s := newStore(t, newID("conv"))
	base := time.Now().UTC().Truncate(time.Microsecond)
	first := types.Checkpoint{ID: types.CheckpointID(newID("cp")), Branch: "main", NodeID: "n1", Name: "one", CreatedAt: base}
	second := types.Checkpoint{ID: types.CheckpointID(newID("cp")), Branch: "main", NodeID: "n2", Name: "two", CreatedAt: base.Add(time.Second)}
	for _, cp := range []types.Checkpoint{second, first} {
		if err := s.SaveCheckpoint(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadCheckpoint(ctx, first.ID)
	if err != nil || got.NodeID != "n1" || got.Name != "one" {
		t.Errorf("LoadCheckpoint = %+v, %v", got, err)
	}
	list, err := s.ListCheckpoints(ctx)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Errorf("ListCheckpoints = %+v, %v; want creation order", list, err)
	}
}

func testTxRollback(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx types.StoreTx) error {
		if err := tx.SaveNode(ctx, node(root, "", 0, 1, "root")); err != nil {
			return err
		}
		if err := tx.SaveBranch(ctx, "main", root); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx error = %v, want boom", err)
	}
	if _, err := s.LoadNode(ctx, root); err == nil {
		t.Error("rolled-back node was persisted")
	}
	if _, err := s.LoadBranch(ctx, "main"); err == nil {
		t.Error("rolled-back branch was persisted")
	}
}

func testTxStaleVersion(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	s := newStore(t, newID("conv"))
	root := types.NodeID(newID("root"))
	save(t, s, node(root, "", 0, 2, "v2"))
	err := s.Tx(ctx, func(tx types.StoreTx) error {
		return tx.SaveNode(ctx, node(root, "", 0, 1, "v1"))
	})
	if !errors.Is(err, tree.ErrVersionConflict) {
		t.Fatalf("Tx stale SaveNode error = %v, want ErrVersionConflict", err)
	}
}

func testCrossConversation(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	convA, convB := newID("conv-a"), newID("conv-b")
	a := newStore(t, convA)
	b := newStore(t, convB)
	root := types.NodeID(newID("root"))
	child := types.NodeID(newID("child"))
	save(t, a, node(root, "", 0, 1, "root"), node(child, root, 1, 1, "child"))
	if _, err := b.LoadNode(ctx, root); err == nil {
		t.Error("conversation B loaded a node of conversation A")
	}
	if kids, err := b.LoadChildren(ctx, root); err != nil || len(kids) != 0 {
		t.Errorf("conversation B children of A's root = %v, %v", ids(kids), err)
	}
	if nodes, _, err := b.LoadTree(ctx, root); err == nil && len(nodes) > 0 {
		t.Errorf("conversation B loaded A's tree: %v", ids(nodes))
	}
	if path, err := b.LoadPath(ctx, child); err == nil && len(path) > 0 {
		t.Errorf("conversation B loaded A's path: %v", ids(path))
	}
	if _, err := a.LoadNode(ctx, root); err != nil {
		t.Errorf("conversation A lost its own node: %v", err)
	}
}

func testActiveBranch(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	s := newStore(t, newID("conv"))
	r, rok := s.(tree.ActiveBranchReader)
	w, wok := s.(tree.ActiveBranchWriter)
	if !rok || !wok {
		t.Skip("store does not persist the active branch")
	}
	if got, err := r.LoadActiveBranch(ctx); err != nil || got != "" {
		t.Errorf("unset active branch = %q, %v; want empty", got, err)
	}
	if err := w.SaveActiveBranch(ctx, "side"); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(ctx, func(tx types.StoreTx) error {
		tw, ok := tx.(tree.ActiveBranchWriter)
		if !ok {
			t.Error("store transaction does not persist the active branch")
			return nil
		}
		return tw.SaveActiveBranch(ctx, "compact-main")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.LoadActiveBranch(ctx); err != nil || got != "compact-main" {
		t.Errorf("active branch = %q, %v; want compact-main", got, err)
	}
	other := newStore(t, newID("conv"))
	if or, ok := other.(tree.ActiveBranchReader); ok {
		if got, _ := or.LoadActiveBranch(ctx); got != "" {
			t.Errorf("active branch leaked across conversations: %q", got)
		}
	}
}

func testDeleteConversation(t *testing.T, newStore NewStore) {
	ctx := context.Background()
	convA, convB := newID("conv-a"), newID("conv-b")
	a := newStore(t, convA)
	b := newStore(t, convB)
	del, ok := a.(ConversationDeleter)
	if !ok {
		t.Skip("store does not support DeleteConversation")
	}
	rootA, rootB := types.NodeID(newID("root")), types.NodeID(newID("root"))
	save(t, a, node(rootA, "", 0, 1, "a"))
	save(t, b, node(rootB, "", 0, 1, "b"))
	for _, s := range []struct {
		store types.Store
		root  types.NodeID
	}{{a, rootA}, {b, rootB}} {
		if err := s.store.SaveBranch(ctx, "main", s.root); err != nil {
			t.Fatal(err)
		}
		cp := types.Checkpoint{ID: types.CheckpointID(newID("cp")), Branch: "main", NodeID: s.root, Name: "cp", CreatedAt: time.Now().UTC()}
		if err := s.store.SaveCheckpoint(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	if err := del.DeleteConversation(ctx); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if _, err := a.LoadNode(ctx, rootA); err == nil {
		t.Error("node survived DeleteConversation")
	}
	if br, _ := a.ListBranches(ctx); len(br) != 0 {
		t.Errorf("branches survived DeleteConversation: %v", br)
	}
	if cps, _ := a.ListCheckpoints(ctx); len(cps) != 0 {
		t.Errorf("checkpoints survived DeleteConversation: %v", cps)
	}
	if _, err := b.LoadNode(ctx, rootB); err != nil {
		t.Errorf("DeleteConversation removed another conversation's node: %v", err)
	}
	if br, _ := b.ListBranches(ctx); len(br) != 1 {
		t.Errorf("DeleteConversation removed another conversation's branches: %v", br)
	}
	if cps, _ := b.ListCheckpoints(ctx); len(cps) != 1 {
		t.Errorf("DeleteConversation removed another conversation's checkpoints: %v", cps)
	}
}
