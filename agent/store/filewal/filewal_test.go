package filewal_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/store/filewal"
	"github.com/urmzd/saige/agent/types"
)

func newWAL(t *testing.T) (*filewal.WAL, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	w, err := filewal.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w, path
}

func sampleNode() *types.Node {
	now := time.Now().UTC().Truncate(time.Millisecond)
	archived := now.Add(time.Minute)
	return &types.Node{
		ID:       "node-1",
		ParentID: "root",
		Message: types.AssistantMessage{Content: []types.AssistantContent{
			types.TextContent{Text: "hello"},
			types.ThinkingContent{Thinking: "hmm"},
		}},
		State:      types.NodeArchived,
		Version:    3,
		Depth:      2,
		BranchID:   "main",
		CreatedAt:  now,
		UpdatedAt:  now,
		ArchivedAt: &archived,
		ArchivedBy: "tester",
		SummaryOf:  []types.NodeID{"a", "b"},
	}
}

func commitTx(t *testing.T, w *filewal.WAL, ops ...types.TxOp) types.TxID {
	t.Helper()
	ctx := context.Background()
	txID, err := w.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, op := range ops {
		if err := w.Append(ctx, txID, op); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return txID
}

func TestFileWALRoundTrip(t *testing.T) {
	ctx := context.Background()
	w, path := newWAL(t)

	node := sampleNode()
	cp := &types.Checkpoint{ID: "cp-1", Branch: "main", NodeID: "node-1", Name: "snap",
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	txID := commitTx(t, w,
		types.TxOp{Kind: types.TxOpAddNode, NodeID: node.ID, ParentID: node.ParentID, Node: node},
		types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: node.ID},
		types.TxOp{Kind: types.TxOpAddCheckpoint, Checkpoint: cp},
	)

	// Reopen from disk to prove durability, not in-memory state.
	w.Close()
	reopened, err := filewal.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	committed, err := reopened.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(committed) != 1 || committed[0] != txID {
		t.Fatalf("Recover = %v, want [%s]", committed, txID)
	}

	ops, err := reopened.Replay(ctx, txID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("ops = %d, want 3", len(ops))
	}

	got := ops[0].Node
	if got == nil {
		t.Fatal("node op lost its node")
	}
	if got.ID != node.ID || got.State != node.State || got.Version != node.Version ||
		got.BranchID != node.BranchID || got.ArchivedBy != node.ArchivedBy ||
		len(got.SummaryOf) != 2 {
		t.Errorf("node round-trip mismatch:\n got %+v\nwant %+v", got, node)
	}
	if got.ArchivedAt == nil || !got.ArchivedAt.Equal(*node.ArchivedAt) {
		t.Errorf("ArchivedAt round-trip: got %v want %v", got.ArchivedAt, node.ArchivedAt)
	}
	am, ok := got.Message.(types.AssistantMessage)
	if !ok || len(am.Content) != 2 {
		t.Fatalf("message round-trip: %#v", got.Message)
	}
	if txt, ok := am.Content[0].(types.TextContent); !ok || txt.Text != "hello" {
		t.Errorf("text content round-trip: %#v", am.Content[0])
	}

	if ops[1].Kind != types.TxOpSetBranch || ops[1].BranchID != "main" || ops[1].TipID != node.ID {
		t.Errorf("set_branch round-trip: %+v", ops[1])
	}
	if ops[2].Checkpoint == nil || ops[2].Checkpoint.ID != cp.ID || ops[2].Checkpoint.Name != cp.Name {
		t.Errorf("checkpoint round-trip: %+v", ops[2].Checkpoint)
	}
}

func TestFileWALAbortAndUncommittedNotRecovered(t *testing.T) {
	ctx := context.Background()
	w, _ := newWAL(t)

	committedTx := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})

	// Aborted tx.
	aborted, _ := w.Begin(ctx)
	_ = w.Append(ctx, aborted, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "x", TipID: "n2"})
	if err := w.Abort(ctx, aborted); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	// Begun but never committed ("crash before commit").
	open, _ := w.Begin(ctx)
	_ = w.Append(ctx, open, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "y", TipID: "n3"})

	got, err := w.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(got) != 1 || got[0] != committedTx {
		t.Fatalf("Recover = %v, want only %s", got, committedTx)
	}

	// Finalized txs reject further ops.
	if err := w.Append(ctx, committedTx, types.TxOp{}); err == nil {
		t.Error("expected error appending to committed tx")
	}
	if err := w.Append(ctx, aborted, types.TxOp{}); err == nil {
		t.Error("expected error appending to aborted tx")
	}
}

func TestFileWALTornFinalLineTolerated(t *testing.T) {
	ctx := context.Background()
	w, path := newWAL(t)

	txID := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})

	// Simulate a crash mid-write: a truncated, non-JSON final line.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"commit","tx":"torn","ops":[{"ki`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	committed, err := w.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover with torn tail: %v", err)
	}
	if len(committed) != 1 || committed[0] != txID {
		t.Fatalf("Recover = %v, want [%s]", committed, txID)
	}
	w.Close()

	// Reopening (the crash-restart path) truncates the torn tail so new
	// commits land on a fresh line and remain readable.
	reopened, err := filewal.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	tx2 := commitTx(t, reopened, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n2"})

	committed, err = reopened.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover after repair: %v", err)
	}
	if len(committed) != 2 || committed[0] != txID || committed[1] != tx2 {
		t.Fatalf("Recover after repair = %v, want [%s %s]", committed, txID, tx2)
	}
}

func TestFileWALMidLogCorruptionErrors(t *testing.T) {
	ctx := context.Background()
	w, path := newWAL(t)

	commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})

	// Corrupt line followed by a valid record — not a torn tail.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("garbage not json\n")
	f.Close()
	commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n2"})

	if _, err := w.Recover(ctx); err == nil {
		t.Fatal("expected error for mid-log corruption")
	}
}

func TestFileWALMarkApplied(t *testing.T) {
	ctx := context.Background()
	w, path := newWAL(t)

	tx1 := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})
	tx2 := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n2"})

	if err := w.MarkApplied(ctx, tx1); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	committed, err := w.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(committed) != 1 || committed[0] != tx2 {
		t.Fatalf("Recover = %v, want only %s", committed, tx2)
	}

	// Applied markers survive reopen.
	w.Close()
	reopened, err := filewal.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	committed, err = reopened.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 1 || committed[0] != tx2 {
		t.Fatalf("Recover after reopen = %v, want only %s", committed, tx2)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// TestFileWALCompact proves compaction shrinks the log while preserving
// exactly the committed-but-unapplied transactions, across a reopen.
func TestFileWALCompact(t *testing.T) {
	ctx := context.Background()
	w, path := newWAL(t)

	tx1 := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})
	tx2 := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n2"})
	node := sampleNode()
	tx3 := commitTx(t, w, types.TxOp{Kind: types.TxOpAddNode, NodeID: node.ID, ParentID: node.ParentID, Node: node})
	if err := w.MarkApplied(ctx, tx1); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	if err := w.MarkApplied(ctx, tx2); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	before := fileSize(t, path)
	if err := w.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after := fileSize(t, path)
	if after >= before {
		t.Errorf("Compact did not shrink log: before=%d after=%d", before, after)
	}

	// Only the unapplied tx survives, and the swapped append handle still
	// works: a post-compact commit lands in the new file.
	tx4 := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n4"})

	w.Close()
	reopened, err := filewal.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	committed, err := reopened.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover after compact: %v", err)
	}
	if len(committed) != 2 || committed[0] != tx3 || committed[1] != tx4 {
		t.Fatalf("Recover after compact = %v, want [%s %s]", committed, tx3, tx4)
	}
	ops, err := reopened.Replay(ctx, tx3)
	if err != nil {
		t.Fatalf("Replay surviving tx: %v", err)
	}
	if len(ops) != 1 || ops[0].Node == nil || ops[0].Node.ID != node.ID {
		t.Fatalf("Replay ops after compact = %+v, want node op for %s", ops, node.ID)
	}
}

// TestFileWALCompactAllApplied is the post-recovery steady state: everything
// applied, so compaction empties the log.
func TestFileWALCompactAllApplied(t *testing.T) {
	ctx := context.Background()
	w, path := newWAL(t)

	tx1 := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})
	if err := w.MarkApplied(ctx, tx1); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	if err := w.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := fileSize(t, path); got != 0 {
		t.Errorf("log size after full compact = %d, want 0", got)
	}
	committed, err := w.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(committed) != 0 {
		t.Fatalf("Recover after full compact = %v, want empty", committed)
	}
}

// TestFileWALCompactCrashLeftoverTempIgnored simulates a crash mid-Compact
// that leaves a temp file behind: New must open the WAL normally and the
// stray file must not affect reads.
func TestFileWALCompactCrashLeftoverTempIgnored(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.jsonl")

	stray := filepath.Join(dir, "wal.jsonl.compact-12345")
	if err := os.WriteFile(stray, []byte(`{"kind":"commit","tx":"half-writ`), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := filewal.New(path)
	if err != nil {
		t.Fatalf("New with stray compact temp: %v", err)
	}
	defer w.Close()

	txID := commitTx(t, w, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "n1"})
	committed, err := w.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(committed) != 1 || committed[0] != txID {
		t.Fatalf("Recover = %v, want [%s]", committed, txID)
	}
}
