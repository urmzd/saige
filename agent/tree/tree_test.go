package tree

import (
	"context"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/store/memwal"
)

func TestNewTree(t *testing.T) {
	tree, err := New(types.NewSystemMessage("You are a helper."))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := tree.Root()
	if root == nil {
		t.Fatal("root is nil")
	}
	if root.Depth != 0 {
		t.Errorf("root depth = %d, want 0", root.Depth)
	}
	if _, ok := root.Message.(types.SystemMessage); !ok {
		t.Error("root message is not SystemMessage")
	}
	branches := tree.Branches()
	if _, ok := branches["main"]; !ok {
		t.Error("main branch not found")
	}
}

func TestAddChildAndPath(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	// Add user message
	user, err := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	if err != nil {
		t.Fatalf("AddChild user: %v", err)
	}
	if user.Depth != 1 {
		t.Errorf("user depth = %d, want 1", user.Depth)
	}

	// Add assistant message
	asst, err := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi there"}},
	})
	if err != nil {
		t.Fatalf("AddChild assistant: %v", err)
	}
	if asst.Depth != 2 {
		t.Errorf("assistant depth = %d, want 2", asst.Depth)
	}

	// Verify path
	path, err := tree.Path(asst.ID)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if len(path) != 3 {
		t.Fatalf("path length = %d, want 3", len(path))
	}
	if path[0] != root.ID {
		t.Error("path[0] != root")
	}
	if path[2] != asst.ID {
		t.Error("path[2] != assistant")
	}
}

func TestFlatten(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	msgs, err := tree.Flatten(asst.ID)
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("flatten length = %d, want 3", len(msgs))
	}
	if msgs[0].Role() != types.RoleSystem {
		t.Error("msgs[0] not system")
	}
	if msgs[1].Role() != types.RoleUser {
		t.Error("msgs[1] not user")
	}
	if msgs[2].Role() != types.RoleAssistant {
		t.Error("msgs[2] not assistant")
	}
}

func TestFlattenBranch(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()
	tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	msgs, err := tree.FlattenBranch("main")
	if err != nil {
		t.Fatalf("FlattenBranch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("flatten branch length = %d, want 2", len(msgs))
	}
}

func TestBranch(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Branch from assistant message
	branchID, branchNode, err := tree.Branch(context.Background(), asst.ID, "alt", types.NewUserMessage("different question"))
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if branchNode.Depth != 3 {
		t.Errorf("branch node depth = %d, want 3", branchNode.Depth)
	}

	// Both branches should flatten correctly
	mainMsgs, _ := tree.FlattenBranch("main")
	altMsgs, _ := tree.FlattenBranch(branchID)

	if len(mainMsgs) != 3 {
		t.Errorf("main branch length = %d, want 3", len(mainMsgs))
	}
	if len(altMsgs) != 4 {
		t.Errorf("alt branch length = %d, want 4", len(altMsgs))
	}

	// Verify alt branch shares the path up to the branch point
	mainPath, _ := tree.Path(tree.branches["main"])
	altPath, _ := tree.Path(branchNode.ID)
	// First 3 nodes should be the same (root, user, assistant)
	for i := 0; i < 3; i++ {
		if mainPath[i] != altPath[i] {
			t.Errorf("path[%d] differs between branches", i)
		}
	}
}

func TestUpdateUserMessage(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("original"))

	newBranch, newNode, err := tree.UpdateUserMessage(context.Background(), user.ID, types.NewUserMessage("edited"))
	if err != nil {
		t.Fatalf("UpdateUserMessage: %v", err)
	}
	if newNode.Depth != 1 {
		t.Errorf("new node depth = %d, want 1", newNode.Depth)
	}

	// Both branches exist and flatten correctly
	mainMsgs, _ := tree.FlattenBranch("main")
	editMsgs, _ := tree.FlattenBranch(newBranch)

	if len(mainMsgs) != 2 {
		t.Errorf("main length = %d, want 2", len(mainMsgs))
	}
	if len(editMsgs) != 2 {
		t.Errorf("edit length = %d, want 2", len(editMsgs))
	}

	// Check the edited message content
	um, ok := editMsgs[1].(types.UserMessage)
	if !ok {
		t.Fatal("edit msgs[1] not UserMessage")
	}
	tc, ok := um.Content[0].(types.TextContent)
	if !ok {
		t.Fatal("edit msg content not TextContent")
	}
	if tc.Text != "edited" {
		t.Errorf("edit msg text = %q, want %q", tc.Text, "edited")
	}
}

func TestUpdateUserMessage_NotUserMessage(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	asst, _ := tree.AddChild(context.Background(), root.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	_, _, err := tree.UpdateUserMessage(context.Background(), asst.ID, types.NewUserMessage("edited"))
	if err == nil {
		t.Fatal("expected error for non-user message")
	}
}

func TestUpdateUserMessage_Root(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	_, _, err := tree.UpdateUserMessage(context.Background(), root.ID, types.NewUserMessage("edited"))
	if err == nil {
		t.Fatal("expected error for root update")
	}
}

func TestArchiveAndRestore(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Archive the user node recursively
	if err := tree.Archive(user.ID, "test", true); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Flatten should skip archived nodes
	msgs, _ := tree.Flatten(asst.ID)
	if len(msgs) != 1 { // only root
		t.Errorf("flatten after archive = %d, want 1", len(msgs))
	}

	// Restore
	if err := tree.Restore(user.ID, true); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	msgs, _ = tree.Flatten(asst.ID)
	if len(msgs) != 3 {
		t.Errorf("flatten after restore = %d, want 3", len(msgs))
	}
}

func TestArchiveRoot(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	err := tree.Archive(root.ID, "test", false)
	if err == nil {
		t.Fatal("expected error archiving root")
	}
}

func TestCheckpointAndRewind(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Checkpoint at current tip
	cpID, err := tree.Checkpoint("main", "save1")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Add more messages
	tip, _ := tree.Tip("main")
	tree.AddChild(context.Background(), tip.ID, types.NewUserMessage("more stuff"))

	mainMsgs, _ := tree.FlattenBranch("main")
	if len(mainMsgs) != 4 {
		t.Errorf("main after more = %d, want 4", len(mainMsgs))
	}

	// Rewind
	rewindBranch, err := tree.Rewind(cpID)
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}

	rewindMsgs, _ := tree.FlattenBranch(rewindBranch)
	if len(rewindMsgs) != 3 {
		t.Errorf("rewind branch = %d, want 3", len(rewindMsgs))
	}
}

func TestChildren(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	tree.AddChild(context.Background(), root.ID, types.NewUserMessage("a"))
	tree.AddChild(context.Background(), root.ID, types.NewUserMessage("b"))

	children, err := tree.Children(root.ID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(children) != 2 {
		t.Errorf("children count = %d, want 2", len(children))
	}
}

func TestNodeNotFound(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))

	_, err := tree.AddChild(context.Background(), "nonexistent", types.NewUserMessage("hello"))
	if err == nil {
		t.Fatal("expected ErrNodeNotFound")
	}
}

func TestTipBranchNotFound(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))

	_, err := tree.Tip("nonexistent")
	if err == nil {
		t.Fatal("expected ErrBranchNotFound")
	}
}

func TestConcurrentBranchWrites(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Concurrent writes to different branches should succeed.
	var wg sync.WaitGroup
	errors := make([]error, 10)
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := tree.Branch(context.Background(), asst.ID, "concurrent", types.NewUserMessage("branch"))
			errors[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("branch %d failed: %v", i, err)
		}
	}

	// Should have main + 10 concurrent branches
	branches := tree.Branches()
	if len(branches) != 11 {
		t.Errorf("branch count = %d, want 11", len(branches))
	}
}

func TestTreeWithWAL(t *testing.T) {
	wal := memwal.New()
	tree, _ := New(types.NewSystemMessage("system"), WithWAL(wal))
	root := tree.Root()

	tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	// Verify WAL recorded transactions
	committed, _ := wal.Recover(context.Background())
	if len(committed) != 1 {
		t.Errorf("committed txns = %d, want 1", len(committed))
	}

	ops, _ := wal.Replay(context.Background(), committed[0])
	if len(ops) != 1 {
		t.Errorf("ops = %d, want 1", len(ops))
	}
	if ops[0].Kind != types.TxOpAddNode {
		t.Errorf("op kind = %s, want %s", ops[0].Kind, types.TxOpAddNode)
	}
}

func TestAddChildToArchivedNode(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	tree.Archive(user.ID, "test", false)

	_, err := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error adding child to archived node")
	}
}

// mockProvider implements types.Provider for testing.
type mockProvider struct {
	response string
}

func (m *mockProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: m.response}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

// mockTokenizer implements types.Tokenizer for testing.
type mockTokenizer struct {
	tokensPerMessage int
}

func (m *mockTokenizer) CountTokens(_ context.Context, messages []types.Message) (int, error) {
	return len(messages) * m.tokensPerMessage, nil
}

func TestCompaction(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	// Build a conversation with several turns.
	current := root
	for i := range 6 {
		var msg types.Message
		if i%2 == 0 {
			msg = types.NewUserMessage("user message")
		} else {
			msg = types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "assistant reply"}}}
		}
		node, err := tree.AddChild(context.Background(), current.ID, msg)
		if err != nil {
			t.Fatalf("AddChild %d: %v", i, err)
		}
		current = node
	}

	provider := &mockProvider{response: "summarized content"}
	tokenizer := &mockTokenizer{tokensPerMessage: 100} // 7 messages * 100 = 700 tokens

	newBranch, err := tree.Compact(context.Background(), "main", provider, tokenizer, CompactOpts{
		MaxTokens:      500, // under 700, so compaction fires
		PreserveShared: true,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if newBranch == "main" {
		t.Fatal("expected new branch, got main")
	}

	// New branch should be active.
	if tree.Active() != newBranch {
		t.Errorf("active = %s, want %s", tree.Active(), newBranch)
	}

	// Flatten the new branch should produce valid messages.
	msgs, err := tree.FlattenBranch(newBranch)
	if err != nil {
		t.Fatalf("FlattenBranch after compact: %v", err)
	}
	if len(msgs) < 1 {
		t.Error("flatten after compact produced no messages")
	}

	// Old branch should still be intact.
	oldMsgs, err := tree.FlattenBranch("main")
	if err != nil {
		t.Fatalf("FlattenBranch main after compact: %v", err)
	}
	if len(oldMsgs) != 7 { // root + 6 messages
		t.Errorf("old branch messages = %d, want 7", len(oldMsgs))
	}
}

func TestCompactionUnderBudget(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	provider := &mockProvider{response: "summary"}
	tokenizer := &mockTokenizer{tokensPerMessage: 10} // 2 messages * 10 = 20 tokens

	branch, err := tree.Compact(context.Background(), "main", provider, tokenizer, CompactOpts{
		MaxTokens:      100, // well over 20, no compaction needed
		PreserveShared: true,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if branch != "main" {
		t.Errorf("expected same branch 'main', got %s", branch)
	}
}

func TestCompactionPreservesShared(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	// Build shared history.
	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("shared question"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "shared answer"}},
	})

	// Create two branches from the shared assistant message.
	branchA, _, _ := tree.Branch(context.Background(), asst.ID, "branchA", types.NewUserMessage("branch A"))
	tree.Branch(context.Background(), asst.ID, "branchB", types.NewUserMessage("branch B"))

	provider := &mockProvider{response: "summary"}
	tokenizer := &mockTokenizer{tokensPerMessage: 100} // over budget

	_, err := tree.Compact(context.Background(), branchA, provider, tokenizer, CompactOpts{
		MaxTokens:      200,
		PreserveShared: true,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Shared nodes (user, asst) should still be active since they're on branchB's path too.
	tree.mu.RLock()
	userNode := tree.nodes[user.ID]
	asstNode := tree.nodes[asst.ID]
	tree.mu.RUnlock()

	if userNode.State != types.NodeActive {
		t.Error("shared user node was compacted")
	}
	if asstNode.State != types.NodeActive {
		t.Error("shared assistant node was compacted")
	}
}

func TestInMemoryWAL(t *testing.T) {
	wal := memwal.New()

	ctx := context.Background()

	// Begin and commit
	txID, err := wal.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	err = wal.Append(ctx, txID, types.TxOp{Kind: types.TxOpAddNode})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	err = wal.Commit(ctx, txID)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Begin and abort
	txID2, _ := wal.Begin(ctx)
	wal.Append(ctx, txID2, types.TxOp{Kind: types.TxOpSetBranch})
	wal.Abort(ctx, txID2)

	// Recover should only return committed
	committed, _ := wal.Recover(ctx)
	if len(committed) != 1 {
		t.Errorf("committed = %d, want 1", len(committed))
	}

	// Replay
	ops, _ := wal.Replay(ctx, committed[0])
	if len(ops) != 1 {
		t.Errorf("ops = %d, want 1", len(ops))
	}

	// Append to committed tx should fail
	err = wal.Append(ctx, txID, types.TxOp{Kind: types.TxOpAddNode})
	if err == nil {
		t.Fatal("expected error appending to committed tx")
	}
}

func TestCheckpointNotFound(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))

	_, err := tree.Rewind("nonexistent")
	if err == nil {
		t.Fatal("expected ErrCheckpointNotFound")
	}
}

// -- TreePath Tests --

func TestTreePathString(t *testing.T) {
	tests := []struct {
		path types.TreePath
		want string
	}{
		{types.TreePath{}, ""},
		{types.TreePath{0}, "0"},
		{types.TreePath{0, 1, 2}, "0/1/2"},
		{types.TreePath{3, 14, 159}, "3/14/159"},
	}
	for _, tt := range tests {
		got := tt.path.String()
		if got != tt.want {
			t.Errorf("TreePath(%v).String() = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestTreePathRoundTrip(t *testing.T) {
	paths := []types.TreePath{
		{},
		{0},
		{0, 1, 2},
		{10, 20, 30},
	}
	for _, p := range paths {
		s := p.String()
		parsed, err := types.ParseTreePath(s)
		if err != nil {
			t.Fatalf("ParseTreePath(%q): %v", s, err)
		}
		if len(parsed) != len(p) {
			t.Errorf("round-trip mismatch: %v -> %q -> %v", p, s, parsed)
			continue
		}
		for i := range p {
			if parsed[i] != p[i] {
				t.Errorf("round-trip mismatch at [%d]: %v -> %q -> %v", i, p, s, parsed)
			}
		}
	}
}

func TestParseTreePathError(t *testing.T) {
	_, err := types.ParseTreePath("0/abc/2")
	if err == nil {
		t.Fatal("expected error for invalid tree path")
	}
}

func TestTreePathParent(t *testing.T) {
	if p := (types.TreePath{0, 1, 2}).Parent(); p.String() != "0/1" {
		t.Errorf("Parent() = %v, want 0/1", p)
	}
	if p := (types.TreePath{0}).Parent(); p != nil {
		t.Errorf("Parent() of single = %v, want nil", p)
	}
	if p := (types.TreePath{}).Parent(); p != nil {
		t.Errorf("Parent() of empty = %v, want nil", p)
	}
}

func TestTreePathIsAncestorOf(t *testing.T) {
	tests := []struct {
		a, b types.TreePath
		want bool
	}{
		{types.TreePath{0}, types.TreePath{0, 1}, true},
		{types.TreePath{0, 1}, types.TreePath{0, 1, 2}, true},
		{types.TreePath{0, 1, 2}, types.TreePath{0, 1}, false},     // not strict prefix
		{types.TreePath{0, 1}, types.TreePath{0, 1}, false},         // equal, not strict
		{types.TreePath{0, 2}, types.TreePath{0, 1, 2}, false},      // divergent
		{types.TreePath{}, types.TreePath{0}, true},                  // root is ancestor of everything
		{types.TreePath{}, types.TreePath{}, false},                  // empty not ancestor of itself
	}
	for _, tt := range tests {
		got := tt.a.IsAncestorOf(tt.b)
		if got != tt.want {
			t.Errorf("%v.IsAncestorOf(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// -- NodePath Tests --

func TestNodePath(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	// root path should be empty
	rootPath, err := tree.NodePath(root.ID)
	if err != nil {
		t.Fatalf("NodePath(root): %v", err)
	}
	if len(rootPath) != 0 {
		t.Errorf("root path = %v, want empty", rootPath)
	}

	// Add two children to root
	a, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("a"))
	b, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("b"))

	aPath, _ := tree.NodePath(a.ID)
	if aPath.String() != "0" {
		t.Errorf("a path = %v, want 0", aPath)
	}

	bPath, _ := tree.NodePath(b.ID)
	if bPath.String() != "1" {
		t.Errorf("b path = %v, want 1", bPath)
	}

	// Add child to a
	c, _ := tree.AddChild(context.Background(), a.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "c"}},
	})

	cPath, _ := tree.NodePath(c.ID)
	if cPath.String() != "0/0" {
		t.Errorf("c path = %v, want 0/0", cPath)
	}
}

func TestNodePathAfterBranch(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Branch from assistant
	_, branchNode, _ := tree.Branch(context.Background(), asst.ID, "alt", types.NewUserMessage("different"))

	branchPath, err := tree.NodePath(branchNode.ID)
	if err != nil {
		t.Fatalf("NodePath(branch): %v", err)
	}
	if branchPath.String() != "0/0/0" {
		t.Errorf("branch path = %v, want 0/0/0", branchPath)
	}
}

// -- FlattenAnnotated Tests --

func TestFlattenAnnotated(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	annotated, err := tree.FlattenAnnotated(asst.ID)
	if err != nil {
		t.Fatalf("FlattenAnnotated: %v", err)
	}
	if len(annotated) != 3 {
		t.Fatalf("annotated length = %d, want 3", len(annotated))
	}

	// Check root
	if annotated[0].NodeID != root.ID {
		t.Error("annotated[0] != root")
	}
	if annotated[0].Path.String() != "" {
		t.Errorf("root path = %v, want empty", annotated[0].Path)
	}
	if annotated[0].Depth != 0 {
		t.Errorf("root depth = %d, want 0", annotated[0].Depth)
	}

	// Check user
	if annotated[1].NodeID != user.ID {
		t.Error("annotated[1] != user")
	}
	if annotated[1].Path.String() != "0" {
		t.Errorf("user path = %v, want 0", annotated[1].Path)
	}
	if annotated[1].Depth != 1 {
		t.Errorf("user depth = %d, want 1", annotated[1].Depth)
	}

	// Check assistant
	if annotated[2].NodeID != asst.ID {
		t.Error("annotated[2] != asst")
	}
	if annotated[2].Path.String() != "0/0" {
		t.Errorf("asst path = %v, want 0/0", annotated[2].Path)
	}
}

func TestFlattenAnnotatedSkipsArchived(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	tree.Archive(user.ID, "test", false)

	annotated, err := tree.FlattenAnnotated(asst.ID)
	if err != nil {
		t.Fatalf("FlattenAnnotated: %v", err)
	}
	// Should skip archived user node: root + asst
	if len(annotated) != 2 {
		t.Fatalf("annotated length = %d, want 2", len(annotated))
	}
}

func TestFlattenBranchAnnotated(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()
	tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	annotated, err := tree.FlattenBranchAnnotated("main")
	if err != nil {
		t.Fatalf("FlattenBranchAnnotated: %v", err)
	}
	if len(annotated) != 2 {
		t.Fatalf("annotated length = %d, want 2", len(annotated))
	}
}

// -- Diff Tests --

func TestDiffSameNode(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	diff, err := tree.Diff(user.ID, user.ID)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.Added) != 0 {
		t.Errorf("added = %d, want 0", len(diff.Added))
	}
	if len(diff.Removed) != 0 {
		t.Errorf("removed = %d, want 0", len(diff.Removed))
	}
	if diff.CommonAncestor != user.ID {
		t.Error("common ancestor should be the node itself")
	}
}

func TestDiffBranches(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	asst, _ := tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	// Create branch from assistant
	branchID, _, _ := tree.Branch(context.Background(), asst.ID, "alt", types.NewUserMessage("different question"))

	// Add more to main
	tip, _ := tree.Tip("main")
	tree.AddChild(context.Background(), tip.ID, types.NewUserMessage("more on main"))

	diff, err := tree.DiffBranches("main", branchID)
	if err != nil {
		t.Fatalf("DiffBranches: %v", err)
	}

	if diff.CommonAncestor != asst.ID {
		t.Error("expected common ancestor to be the assistant node")
	}
	if len(diff.Removed) != 1 { // "more on main"
		t.Errorf("removed = %d, want 1", len(diff.Removed))
	}
	if len(diff.Added) != 1 { // "different question"
		t.Errorf("added = %d, want 1", len(diff.Added))
	}
}

func TestDiffAfterCheckpointAndRewind(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	tree.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})

	cpID, _ := tree.Checkpoint("main", "save1")

	// Add more to main
	tip, _ := tree.Tip("main")
	tree.AddChild(context.Background(), tip.ID, types.NewUserMessage("divergent"))

	rewindBranch, _ := tree.Rewind(cpID)

	diff, err := tree.DiffBranches("main", rewindBranch)
	if err != nil {
		t.Fatalf("DiffBranches: %v", err)
	}

	// main has one extra node after the checkpoint
	if len(diff.Removed) != 1 {
		t.Errorf("removed = %d, want 1", len(diff.Removed))
	}
	if len(diff.Added) != 0 {
		t.Errorf("added = %d, want 0", len(diff.Added))
	}
}

func TestDiffBranchNotFound(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))

	_, err := tree.DiffBranches("main", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent branch")
	}
}

// -- Active Cursor Tests --

func TestActiveDefault(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	if tree.Active() != "main" {
		t.Errorf("default active = %s, want main", tree.Active())
	}
}

func TestSetActive(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))
	root := tree.Root()

	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	branchID, _, _ := tree.Branch(context.Background(), user.ID, "alt", types.NewUserMessage("alt"))

	if err := tree.SetActive(branchID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if tree.Active() != branchID {
		t.Errorf("active = %s, want %s", tree.Active(), branchID)
	}
}

func TestSetActiveNotFound(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("system"))

	err := tree.SetActive("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent branch")
	}
}

func TestArchiveVersionIncrement(t *testing.T) {
	tree, _ := New(types.NewSystemMessage("sys"))
	root := tree.Root()
	user, _ := tree.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	tree.mu.RLock()
	initialVersion := tree.nodes[user.ID].Version
	tree.mu.RUnlock()

	tree.Archive(user.ID, "test", false)

	tree.mu.RLock()
	archivedVersion := tree.nodes[user.ID].Version
	tree.mu.RUnlock()

	if archivedVersion != initialVersion+1 {
		t.Errorf("version after archive = %d, want %d", archivedVersion, initialVersion+1)
	}

	tree.Restore(user.ID, false)

	tree.mu.RLock()
	restoredVersion := tree.nodes[user.ID].Version
	tree.mu.RUnlock()

	if restoredVersion != archivedVersion+1 {
		t.Errorf("version after restore = %d, want %d", restoredVersion, archivedVersion+1)
	}
}
