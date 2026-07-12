package walrecover_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/agent/store/filewal"
	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/store/memwal"
	"github.com/urmzd/saige/agent/store/walrecover"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// buildTree drives a WAL-attached tree through the mutations under test and
// returns the tree plus IDs needed for assertions.
func buildTree(t *testing.T, wal types.WAL) (*tree.Tree, types.CheckpointID) {
	t.Helper()
	ctx := context.Background()

	tr, err := tree.New(types.NewSystemMessage("system"), tree.WithWAL(wal))
	if err != nil {
		t.Fatalf("tree.New: %v", err)
	}
	root := tr.Root()
	user, err := tr.AddChild(ctx, root.ID, types.NewUserMessage("hello"))
	if err != nil {
		t.Fatalf("AddChild: %v", err)
	}
	asst, err := tr.AddChild(ctx, user.ID, types.AssistantMessage{
		Content: []types.AssistantContent{types.TextContent{Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("AddChild: %v", err)
	}
	cpID, err := tr.Checkpoint("main", "snap")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := tr.Archive(asst.ID, "tester", false); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	return tr, cpID
}

// verifyStore asserts the store contents rebuilt from the WAL match the tree.
func verifyStore(t *testing.T, s *memstore.Store, tr *tree.Tree, cpID types.CheckpointID) {
	t.Helper()
	ctx := context.Background()
	root := tr.Root()

	nodes, branches, err := s.LoadTree(ctx, root.ID)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("recovered nodes = %d, want 3", len(nodes))
	}
	wantTip, _ := tr.Tip("main")
	if branches["main"] != wantTip.ID {
		t.Errorf("recovered main tip = %s, want %s", branches["main"], wantTip.ID)
	}

	// The archive mutation (TxOpUpdateNode) must have been applied.
	archived, err := s.LoadNode(ctx, wantTip.ID)
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if archived.State != types.NodeArchived || archived.Version != 2 {
		t.Errorf("archived node not updated in store: state=%d version=%d", archived.State, archived.Version)
	}

	cp, err := s.LoadCheckpoint(ctx, cpID)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if cp.Branch != "main" || cp.NodeID != wantTip.ID {
		t.Errorf("recovered checkpoint = %+v", cp)
	}

	// The recovered data must be enough to rebuild a working tree.
	cps, err := s.ListCheckpoints(ctx)
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	cpMap := make(map[types.CheckpointID]types.Checkpoint, len(cps))
	for _, c := range cps {
		cpMap[c.ID] = c
	}
	rebuilt, err := tree.FromStore(nodes, branches, cpMap, root.ID, "main")
	if err != nil {
		t.Fatalf("FromStore: %v", err)
	}
	if _, err := rebuilt.Rewind(cpID); err != nil {
		t.Errorf("Rewind on rebuilt tree: %v", err)
	}
}

func TestRecoverWALAppliesToMemstore(t *testing.T) {
	ctx := context.Background()
	wal := memwal.New()
	tr, cpID := buildTree(t, wal)

	s := memstore.New()
	applied, err := walrecover.RecoverWAL(ctx, wal, s)
	if err != nil {
		t.Fatalf("RecoverWAL: %v", err)
	}
	// root, user, asst, checkpoint, archive = 5 committed txs.
	if applied != 5 {
		t.Errorf("applied = %d, want 5", applied)
	}

	verifyStore(t, s, tr, cpID)

	// Recovery marks txs applied, so a second pass is a no-op.
	applied, err = walrecover.RecoverWAL(ctx, wal, s)
	if err != nil {
		t.Fatalf("second RecoverWAL: %v", err)
	}
	if applied != 0 {
		t.Errorf("second pass applied = %d, want 0", applied)
	}
}

func TestRecoverWALFromFileWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	wal, err := filewal.New(path)
	if err != nil {
		t.Fatalf("filewal.New: %v", err)
	}
	tr, cpID := buildTree(t, wal)
	wal.Close()

	// "Restart": reopen the log and heal an empty store from it.
	reopened, err := filewal.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	s := memstore.New()
	applied, err := walrecover.RecoverWAL(ctx, reopened, s)
	if err != nil {
		t.Fatalf("RecoverWAL: %v", err)
	}
	if applied != 5 {
		t.Errorf("applied = %d, want 5", applied)
	}

	verifyStore(t, s, tr, cpID)

	applied, err = walrecover.RecoverWAL(ctx, reopened, s)
	if err != nil {
		t.Fatalf("second RecoverWAL: %v", err)
	}
	if applied != 0 {
		t.Errorf("second pass applied = %d, want 0", applied)
	}
}

// TestRecoverWALCompactsFileWAL proves the growth contract: a session's WAL
// accumulates the full history, and a recovery pass applies it, marks it, and
// compacts the log down to (here) nothing: without losing store content.
func TestRecoverWALCompactsFileWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	wal, err := filewal.New(path)
	if err != nil {
		t.Fatalf("filewal.New: %v", err)
	}
	tr, cpID := buildTree(t, wal)
	wal.Close()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before recovery: %v", err)
	}
	if before.Size() == 0 {
		t.Fatal("session left an empty WAL; test cannot observe compaction")
	}

	reopened, err := filewal.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	s := memstore.New()
	if _, err := walrecover.RecoverWAL(ctx, reopened, s); err != nil {
		t.Fatalf("RecoverWAL: %v", err)
	}

	// Recovery ends with Compact: every tx was applied and marked, so the log
	// shrinks to empty instead of replaying O(history) on every startup.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after recovery: %v", err)
	}
	if after.Size() != 0 {
		t.Errorf("log size after recovery = %d, want 0 (before recovery: %d)", after.Size(), before.Size())
	}

	// Store content is unaffected by compaction.
	verifyStore(t, s, tr, cpID)

	// A fresh open of the compacted log recovers nothing.
	reopened.Close()
	final, err := filewal.New(path)
	if err != nil {
		t.Fatalf("open compacted log: %v", err)
	}
	defer final.Close()
	txIDs, err := final.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover on compacted log: %v", err)
	}
	if len(txIDs) != 0 {
		t.Errorf("Recover on compacted log = %v, want empty", txIDs)
	}
	if applied, err := walrecover.RecoverWAL(ctx, final, s); err != nil || applied != 0 {
		t.Errorf("RecoverWAL on compacted log = (%d, %v), want (0, nil)", applied, err)
	}
	verifyStore(t, s, tr, cpID)
}
