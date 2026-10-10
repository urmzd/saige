package tree_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/store/memwal"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

type summaryProvider struct{}

func (summaryProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: "summary"}
	ch <- types.PartEnd{Index: 0}
	close(ch)
	return ch, nil
}

type countTokenizer struct{}

func (countTokenizer) CountTokens(_ context.Context, msgs []types.Message) (int, error) {
	return len(msgs) * 100, nil
}

func assistant(text string) types.AssistantMessage {
	return types.AssistantMessage{Parts: []types.AssistantPart{types.TextPart{Text: text}}}
}

func mustAdd(t *testing.T, tr *tree.Tree, parent types.NodeID, msg types.Message) *types.Node {
	t.Helper()
	n, err := tr.AddChild(context.Background(), parent, msg)
	if err != nil {
		t.Fatalf("AddChild: %v", err)
	}
	return n
}

// TestAddChildAtRewoundTip covers appending at the tip of a rewound branch.
// AddChildOnBranch always extends the named branch. Tip plus AddChild extends
// the rewound branch unless the active branch ends at the same node: AddChild
// only sees the node, so it picks the active branch.
func TestAddChildAtRewoundTip(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mainMovesOn adds a node to main after the checkpoint, so the
		// checkpoint node is mid-branch on main when the rewind is used.
		mainMovesOn bool
		// activate makes the rewound branch active before appending.
		activate bool
		// explicit appends with AddChildOnBranch instead of AddChild.
		explicit bool
		// wantMain expects the child on main: AddChild at a tip shared with
		// the active main branch cannot tell which branch the caller means.
		wantMain bool
	}{
		{name: "main moved on", mainMovesOn: true},
		{name: "main moved on, rewound branch active", mainMovesOn: true, activate: true},
		{name: "shared tip, rewound branch active", activate: true},
		{name: "shared tip, main active, explicit branch", explicit: true},
		{name: "shared tip, main active, AddChild joins main", wantMain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tr, _ := tree.New(types.SystemMsg(types.Text("system")))
			user := mustAdd(t, tr, tr.Root().ID, types.UserMsg(types.Text("q")))
			cpNode := mustAdd(t, tr, user.ID, assistant("a"))
			cp, err := tr.Checkpoint("main", "cp")
			if err != nil {
				t.Fatal(err)
			}
			if tc.mainMovesOn {
				mustAdd(t, tr, cpNode.ID, types.UserMsg(types.Text("main continues")))
			}
			mainTip, _ := tr.Tip("main")

			rewound, err := tr.Rewind(cp)
			if err != nil {
				t.Fatal(err)
			}
			if tc.activate {
				if err := tr.SetActive(rewound); err != nil {
					t.Fatal(err)
				}
			}

			var added *types.Node
			if tc.explicit {
				added, err = tr.AddChildOnBranch(ctx, rewound, types.UserMsg(types.Text("retry")))
			} else {
				tip, tipErr := tr.Tip(rewound)
				if tipErr != nil {
					t.Fatal(tipErr)
				}
				added, err = tr.AddChild(ctx, tip.ID, types.UserMsg(types.Text("retry")))
			}
			if err != nil {
				t.Fatal(err)
			}

			grown, still, stillTip := rewound, types.BranchID("main"), mainTip.ID
			if tc.wantMain {
				grown, still, stillTip = "main", rewound, cpNode.ID
			}
			if added.BranchID != grown {
				t.Errorf("new node branch = %s, want %s", added.BranchID, grown)
			}
			if tip, _ := tr.Tip(grown); tip.ID != added.ID {
				t.Errorf("%s tip = %s, want %s", grown, tip.ID, added.ID)
			}
			if tip, _ := tr.Tip(still); tip.ID != stillTip {
				t.Errorf("%s tip moved to %s, want %s", still, tip.ID, stillTip)
			}
		})
	}
}

func TestAddChildOnBranchUnknownBranch(t *testing.T) {
	tr, _ := tree.New(types.SystemMsg(types.Text("system")))
	_, err := tr.AddChildOnBranch(context.Background(), "missing", types.UserMsg(types.Text("x")))
	if !errors.Is(err, tree.ErrBranchNotFound) {
		t.Fatalf("err = %v, want ErrBranchNotFound", err)
	}
}

// blockingWAL wraps memwal and, once armed, blocks Begin until the context
// is done, like a WAL stuck on slow storage that honors cancellation.
type blockingWAL struct {
	*memwal.WAL
	armed bool
}

func (w *blockingWAL) Begin(ctx context.Context) (types.TxID, error) {
	if w.armed {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return w.WAL.Begin(ctx)
}

// TestContextVariantsStopOnCancelledContext checks that each mutation that
// writes the WAL takes the caller's context, so a cancelled caller is not
// stuck behind a blocked WAL and the tree stays unchanged.
func TestContextVariantsStopOnCancelledContext(t *testing.T) {
	wal := &blockingWAL{WAL: memwal.New()}
	tr, err := tree.New(types.SystemMsg(types.Text("system")), tree.WithWAL(wal))
	if err != nil {
		t.Fatal(err)
	}
	user := mustAdd(t, tr, tr.Root().ID, types.UserMsg(types.Text("q")))
	side, _, err := tr.Branch(context.Background(), tr.Root().ID, "side", types.UserMsg(types.Text("s")))
	if err != nil {
		t.Fatal(err)
	}
	cp, err := tr.Checkpoint("main", "cp")
	if err != nil {
		t.Fatal(err)
	}
	wal.armed = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"NewContext", func() error {
			_, err := tree.NewContext(ctx, types.SystemMsg(types.Text("s")), tree.WithWAL(wal))
			return err
		}},
		{"SetActiveContext", func() error { return tr.SetActiveContext(ctx, side) }},
		{"ArchiveContext", func() error { return tr.ArchiveContext(ctx, user.ID, "tester", true) }},
		{"RestoreContext", func() error { return tr.RestoreContext(ctx, user.ID, true) }},
		{"CheckpointContext", func() error { _, err := tr.CheckpointContext(ctx, "main", "late"); return err }},
		{"RewindContext", func() error { _, err := tr.RewindContext(ctx, cp); return err }},
		{"AddChild", func() error { _, err := tr.AddChild(ctx, user.ID, assistant("a")); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- tc.call() }()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("call blocked on the WAL despite a cancelled context")
			}
		})
	}
	if tr.Active() != "main" || len(tr.Checkpoints()) != 1 || len(tr.Branches()) != 2 {
		t.Errorf("tree changed: active=%s checkpoints=%d branches=%d", tr.Active(), len(tr.Checkpoints()), len(tr.Branches()))
	}
	if n, _ := tr.Tip("main"); n.ID != user.ID || n.State != types.NodeActive {
		t.Errorf("main tip = %+v, want unchanged %s", n, user.ID)
	}
}

// TestWithStoreRoundTrip drives every kind of tree mutation through a tree
// that writes through to a store, then reloads from the store alone and
// compares the result with the live tree.
func TestWithStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	tr, err := tree.New(types.SystemMsg(types.Text("system")), tree.WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	tip := tr.Root()
	for i := range 6 {
		var msg types.Message = types.UserMsg(types.Text("user"))
		if i%2 == 1 {
			msg = assistant("reply")
		}
		tip = mustAdd(t, tr, tip.ID, msg)
	}
	compacted, err := tr.Compact(ctx, "main", summaryProvider{}, countTokenizer{}, tree.CompactOpts{MaxTokens: 500, PreserveShared: true})
	if err != nil || compacted == "main" {
		t.Fatalf("Compact = %s, %v; want a new branch", compacted, err)
	}
	for range 3 {
		if _, err := tr.AddChildOnBranch(ctx, compacted, types.UserMsg(types.Text("after compaction"))); err != nil {
			t.Fatal(err)
		}
	}
	compactTip, _ := tr.Tip(compacted)
	if _, err := tr.AddFeedback(ctx, compactTip.ID, types.UserMsg(types.Text("good"))); err != nil {
		t.Fatal(err)
	}
	mainPath, _ := tr.Path(tip.ID)
	editTarget := mainPath[1]
	if _, _, err := tr.UpdateUserMessage(ctx, editTarget, types.UserMsg(types.Text("edited"))); err != nil {
		t.Fatal(err)
	}
	if err := tr.Archive(mainPath[2], "tester", false); err != nil {
		t.Fatal(err)
	}
	cp, err := tr.Checkpoint(compacted, "cp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Rewind(cp); err != nil {
		t.Fatal(err)
	}
	if tr.Active() != compacted {
		t.Fatalf("active = %s, want %s", tr.Active(), compacted)
	}

	reloaded, err := tree.LoadFromStore(ctx, store, tr.Root().ID, "")
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if reloaded.Active() != compacted {
		t.Errorf("reloaded active = %s, want %s", reloaded.Active(), compacted)
	}
	if !maps.Equal(reloaded.Branches(), tr.Branches()) {
		t.Errorf("branches differ:\nlive     %v\nreloaded %v", tr.Branches(), reloaded.Branches())
	}
	for branch := range tr.Branches() {
		want, _ := tr.FlattenBranch(branch)
		got, err := reloaded.FlattenBranch(branch)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("branch %s differs after reload: %v", branch, err)
		}
	}
	if len(reloaded.Feedback()) != 1 {
		t.Errorf("reloaded feedback = %d, want 1", len(reloaded.Feedback()))
	}
	if !slices.Equal(slices.Sorted(maps.Keys(reloaded.Checkpoints())), slices.Sorted(maps.Keys(tr.Checkpoints()))) {
		t.Errorf("checkpoints differ after reload")
	}
	archived, err := store.LoadNode(ctx, mainPath[2])
	if err != nil || archived.State != types.NodeArchived || archived.ArchivedBy != "tester" {
		t.Errorf("archive state not persisted: %+v, %v", archived, err)
	}
}

type failingStore struct {
	*memstore.Store
	err error
}

func (s failingStore) Tx(context.Context, func(types.StoreTx) error) error { return s.err }

// TestWithStoreFailureLeavesTreeUnchanged checks that a failed store write
// rejects the mutation everywhere: the tree is unchanged and the WAL holds no
// committed transaction a later recovery could replay.
func TestWithStoreFailureLeavesTreeUnchanged(t *testing.T) {
	ctx := context.Background()
	store := &failingStore{Store: memstore.New()}
	wal := memwal.New()
	tr, err := tree.New(types.SystemMsg(types.Text("system")), tree.WithStore(store), tree.WithWAL(wal))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := wal.Recover(ctx)

	store.err = errors.New("db down")
	_, err = tr.AddChild(ctx, tr.Root().ID, types.UserMsg(types.Text("q")))
	if !errors.Is(err, tree.ErrStoreWrite) || !errors.Is(err, store.err) {
		t.Fatalf("err = %v, want ErrStoreWrite wrapping the store error", err)
	}
	if tip, _ := tr.Tip("main"); tip.ID != tr.Root().ID {
		t.Errorf("main tip moved to %s after a failed write", tip.ID)
	}
	after, _ := wal.Recover(ctx)
	if len(after) != len(before) {
		t.Errorf("WAL committed %d transactions for a failed write", len(after)-len(before))
	}
}

// commitFailingWAL wraps memwal and fails every Commit once armed, like a WAL
// on a full disk.
type commitFailingWAL struct {
	*memwal.WAL
	err error
}

func (w *commitFailingWAL) Commit(ctx context.Context, txID types.TxID) error {
	if w.err != nil {
		return w.err
	}
	return w.WAL.Commit(ctx, txID)
}

// TestWALCommitFailure checks how a WAL commit failure surfaces. With a store,
// the store already holds the mutation, so the tree applies it too and returns
// ErrWALCommit; the next append then builds on the stored node instead of
// writing a sibling. Without a store, nothing is durable and the tree stays
// unchanged.
func TestWALCommitFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withStore bool
	}{
		{name: "store and WAL", withStore: true},
		{name: "WAL only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := memstore.New()
			wal := &commitFailingWAL{WAL: memwal.New()}
			opts := []tree.Option{tree.WithWAL(wal)}
			if tc.withStore {
				opts = append(opts, tree.WithStore(store))
			}
			tr, err := tree.New(types.SystemMsg(types.Text("system")), opts...)
			if err != nil {
				t.Fatal(err)
			}

			wal.err = errors.New("disk full")
			node, err := tr.AddChild(ctx, tr.Root().ID, types.UserMsg(types.Text("q")))
			if !errors.Is(err, wal.err) {
				t.Fatalf("err = %v, want the WAL commit error", err)
			}
			if errors.Is(err, tree.ErrWALCommit) != tc.withStore {
				t.Fatalf("errors.Is(err, ErrWALCommit) = %v, want %v", !tc.withStore, tc.withStore)
			}
			tip, _ := tr.Tip("main")
			if !tc.withStore {
				if node != nil || tip.ID != tr.Root().ID {
					t.Fatalf("tree changed after a failed WAL-only write: node=%v tip=%s", node, tip.ID)
				}
				return
			}
			if node == nil || tip.ID != node.ID {
				t.Fatalf("tree did not apply a mutation the store holds: node=%v tip=%s", node, tip.ID)
			}

			wal.err = nil
			next, err := tr.AddChildOnBranch(ctx, "main", assistant("a"))
			if err != nil {
				t.Fatal(err)
			}
			if next.ParentID != node.ID {
				t.Errorf("next parent = %s, want %s", next.ParentID, node.ID)
			}
			reloaded, err := tree.LoadFromStore(ctx, store, tr.Root().ID, "main")
			if err != nil {
				t.Fatal(err)
			}
			if kids, _ := reloaded.Children(tr.Root().ID); len(kids) != 1 {
				t.Errorf("reloaded root has %d children, want 1", len(kids))
			}
			if rtip, _ := reloaded.Tip("main"); rtip.ID != next.ID {
				t.Errorf("reloaded main tip = %s, want %s", rtip.ID, next.ID)
			}
		})
	}
}

// TestWithStoreWriteUsesDetachedContext checks that a store write still
// lands when the caller's context is already cancelled: bookkeeping for work
// that already happened must not depend on the request context.
func TestWithStoreWriteUsesDetachedContext(t *testing.T) {
	store := memstore.New()
	tr, err := tree.New(types.SystemMsg(types.Text("system")), tree.WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	node, err := tr.AddChild(ctx, tr.Root().ID, types.ToolResults(types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("ok")}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadNode(context.Background(), node.ID); err != nil {
		t.Errorf("tool result not persisted: %v", err)
	}
}

func TestLoadFromStoreActiveBranch(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		saved  types.BranchID
		active types.BranchID
		want   types.BranchID
	}{
		{name: "nothing saved", want: "main"},
		{name: "saved branch", saved: "side", want: "side"},
		{name: "saved branch missing", saved: "gone", want: "main"},
		{name: "explicit wins", saved: "side", active: "main", want: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := memstore.New()
			tr, err := tree.New(types.SystemMsg(types.Text("system")), tree.WithStore(store))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := tr.Branch(ctx, tr.Root().ID, "side", types.UserMsg(types.Text("s"))); err != nil {
				t.Fatal(err)
			}
			if tc.saved != "" {
				if err := store.SaveActiveBranch(ctx, tc.saved); err != nil {
					t.Fatal(err)
				}
			}
			reloaded, err := tree.LoadFromStore(ctx, store, tr.Root().ID, tc.active)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Active() != tc.want {
				t.Errorf("active = %s, want %s", reloaded.Active(), tc.want)
			}
		})
	}
}
