package tree

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/store/memwal"
	"github.com/urmzd/saige/agent/types"
)

// lastTxOps returns the ops of the most recently committed WAL transaction,
// asserting exactly wantNew transactions were committed since prev.
func lastTxOps(t *testing.T, wal *memwal.WAL, prev int, wantNew int) []types.TxOp {
	t.Helper()
	ctx := context.Background()
	committed, err := wal.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(committed) - prev; got != wantNew {
		t.Fatalf("new committed txns = %d, want %d", got, wantNew)
	}
	ops, err := wal.Replay(ctx, committed[len(committed)-1])
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return ops
}

func committedCount(t *testing.T, wal *memwal.WAL) int {
	t.Helper()
	committed, err := wal.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return len(committed)
}

func TestArchiveRecursiveWALSingleTx(t *testing.T) {
	wal := memwal.New()
	tr, _ := New(types.NewSystemMessage("system"), WithWAL(wal))
	root := tr.Root()

	// root -> user -> asst -> user2: archiving user recursively mutates 3 nodes.
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("q1"))
	asst, _ := tr.AddChild(context.Background(), user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "a1"}},
	})
	tr.AddChild(context.Background(), asst.ID, types.NewUserMessage("q2"))

	before := committedCount(t, wal)
	if err := tr.Archive(user.ID, "tester", true); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	ops := lastTxOps(t, wal, before, 1)
	if len(ops) != 3 {
		t.Fatalf("archive ops = %d, want 3 (one update per mutated node)", len(ops))
	}
	for i, op := range ops {
		if op.Kind != types.TxOpUpdateNode {
			t.Errorf("op[%d] kind = %s, want %s", i, op.Kind, types.TxOpUpdateNode)
		}
		if op.Node == nil || op.Node.State != types.NodeArchived {
			t.Errorf("op[%d] node not archived: %+v", i, op.Node)
		}
		if op.Node != nil && op.Node.Version != 2 {
			t.Errorf("op[%d] version = %d, want 2", i, op.Node.Version)
		}
	}

	// Restore recursively: another single tx with 3 update ops back to active.
	before = committedCount(t, wal)
	if err := tr.Restore(user.ID, true); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	ops = lastTxOps(t, wal, before, 1)
	if len(ops) != 3 {
		t.Fatalf("restore ops = %d, want 3", len(ops))
	}
	for i, op := range ops {
		if op.Kind != types.TxOpUpdateNode {
			t.Errorf("op[%d] kind = %s, want %s", i, op.Kind, types.TxOpUpdateNode)
		}
		if op.Node == nil || op.Node.State != types.NodeActive || op.Node.ArchivedAt != nil {
			t.Errorf("op[%d] node not restored: %+v", i, op.Node)
		}
	}
}

func TestCheckpointWritesWAL(t *testing.T) {
	wal := memwal.New()
	tr, _ := New(types.NewSystemMessage("system"), WithWAL(wal))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	before := committedCount(t, wal)
	cpID, err := tr.Checkpoint("main", "save1")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	ops := lastTxOps(t, wal, before, 1)
	if len(ops) != 1 || ops[0].Kind != types.TxOpAddCheckpoint {
		t.Fatalf("ops = %+v, want single %s", ops, types.TxOpAddCheckpoint)
	}
	cp := ops[0].Checkpoint
	if cp == nil || cp.ID != cpID || cp.Branch != "main" || cp.NodeID != user.ID {
		t.Errorf("checkpoint op = %+v, want id=%s branch=main node=%s", cp, cpID, user.ID)
	}
}

func TestRewindWritesWAL(t *testing.T) {
	wal := memwal.New()
	tr, _ := New(types.NewSystemMessage("system"), WithWAL(wal))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))
	cpID, _ := tr.Checkpoint("main", "save1")
	tr.AddChild(context.Background(), user.ID, types.NewUserMessage("more"))

	before := committedCount(t, wal)
	rewindBranch, err := tr.Rewind(cpID)
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}

	ops := lastTxOps(t, wal, before, 1)
	if len(ops) != 1 || ops[0].Kind != types.TxOpSetBranch {
		t.Fatalf("ops = %+v, want single %s", ops, types.TxOpSetBranch)
	}
	if ops[0].BranchID != rewindBranch || ops[0].TipID != user.ID {
		t.Errorf("set_branch op = %+v, want %s -> %s", ops[0], rewindBranch, user.ID)
	}
}

func TestSetActiveWritesWAL(t *testing.T) {
	wal := memwal.New()
	tr, _ := New(types.NewSystemMessage("system"), WithWAL(wal))
	root := tr.Root()
	branchID, child, err := tr.Branch(context.Background(), root.ID, "side", types.NewUserMessage("alt"))
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	before := committedCount(t, wal)
	if err := tr.SetActive(branchID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	ops := lastTxOps(t, wal, before, 1)
	if len(ops) != 1 || ops[0].Kind != types.TxOpSetBranch {
		t.Fatalf("ops = %+v, want single %s", ops, types.TxOpSetBranch)
	}
	if ops[0].BranchID != branchID || ops[0].TipID != child.ID {
		t.Errorf("set_branch op = %+v, want %s -> %s", ops[0], branchID, child.ID)
	}
}

func TestCompactWritesOneWALTx(t *testing.T) {
	wal := memwal.New()
	tr, _ := New(types.NewSystemMessage("system"), WithWAL(wal))
	current := tr.Root()
	for i := range 6 {
		var msg types.Message
		if i%2 == 0 {
			msg = types.NewUserMessage("user message")
		} else {
			msg = types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "assistant reply"}}}
		}
		node, err := tr.AddChild(context.Background(), current.ID, msg)
		if err != nil {
			t.Fatalf("AddChild %d: %v", i, err)
		}
		current = node
	}

	before := committedCount(t, wal)
	newBranch, err := tr.Compact(context.Background(), "main",
		&mockProvider{response: "summary"},
		&mockTokenizer{tokensPerMessage: 100},
		CompactOpts{MaxTokens: 500, PreserveShared: true},
	)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if newBranch == "main" {
		t.Fatal("expected compaction to create a new branch")
	}

	ops := lastTxOps(t, wal, before, 1)
	if len(ops) < 2 {
		t.Fatalf("compact ops = %d, want summary node + clones + set_branch", len(ops))
	}
	last := ops[len(ops)-1]
	if last.Kind != types.TxOpSetBranch || last.BranchID != newBranch {
		t.Errorf("final op = %+v, want %s for %s", last, types.TxOpSetBranch, newBranch)
	}
	for i, op := range ops[:len(ops)-1] {
		if op.Kind != types.TxOpAddNode || op.Node == nil {
			t.Errorf("op[%d] = %+v, want %s with node", i, op, types.TxOpAddNode)
		}
	}
	if ops[0].Node.State != types.NodeCompacted {
		t.Errorf("first op node state = %d, want NodeCompacted", ops[0].Node.State)
	}
}
