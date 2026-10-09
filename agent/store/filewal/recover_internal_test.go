package filewal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/store/walrecover"
	"github.com/urmzd/saige/agent/types"
)

// writeLog writes n committed branch-tip transactions straight to a log file,
// skipping the per-commit fsync so large logs build quickly.
func writeLog(t testing.TB, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i := range n {
		rec := record{Kind: recordCommit, Tx: fmt.Sprintf("tx-%d", i), Ops: []walOp{{
			Kind: string(types.TxOpSetBranch), BranchID: "main", TipID: fmt.Sprintf("n%d", i),
		}}}
		if err := enc.Encode(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRecoverWALReadsLogOnce bounds recovery cost: however many transactions
// are pending, RecoverWAL parses the log once to collect them and once more
// to compact it.
func TestRecoverWALReadsLogOnce(t *testing.T) {
	for _, n := range []int{0, 1, walrecover.BatchSize + 1, 5000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := context.Background()
			w, err := New(writeLog(t, n))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			store := memstore.New()

			applied, err := walrecover.RecoverWAL(ctx, w, store)
			if err != nil {
				t.Fatal(err)
			}
			if applied != n {
				t.Errorf("applied = %d, want %d", applied, n)
			}
			if w.reads > 2 {
				t.Errorf("log parsed %d times, want at most 2", w.reads)
			}
			if n > 0 {
				tip, err := store.LoadBranch(ctx, "main")
				if err != nil || tip != types.NodeID(fmt.Sprintf("n%d", n-1)) {
					t.Errorf("main tip = %s, %v; want the last committed tip", tip, err)
				}
			}
			pending, err := w.Recover(ctx)
			if err != nil || len(pending) != 0 {
				t.Errorf("pending after recovery = %v, %v", pending, err)
			}
		})
	}
}

func TestRecoverOpsSkipsApplied(t *testing.T) {
	ctx := context.Background()
	w, err := New(writeLog(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.MarkAppliedBatch(ctx, []types.TxID{"tx-0", "tx-2"}); err != nil {
		t.Fatal(err)
	}
	if err := w.MarkAppliedBatch(ctx, nil); err != nil {
		t.Fatal(err)
	}
	txs, err := w.RecoverOps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []types.TxID
	for _, tx := range txs {
		ids = append(ids, tx.ID)
		if len(tx.Ops) != 1 || tx.Ops[0].Kind != types.TxOpSetBranch {
			t.Errorf("tx %s ops = %+v", tx.ID, tx.Ops)
		}
	}
	if fmt.Sprint(ids) != "[tx-1 tx-3]" {
		t.Errorf("pending = %v, want [tx-1 tx-3]", ids)
	}
	viaRecover, err := w.Recover(ctx)
	if err != nil || fmt.Sprint(viaRecover) != fmt.Sprint(ids) {
		t.Errorf("Recover = %v, %v; want the same set as RecoverOps", viaRecover, err)
	}
}

func BenchmarkRecoverWAL(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				w, err := New(writeLog(b, n))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := walrecover.RecoverWAL(context.Background(), w, memstore.New()); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				_ = w.Close()
				b.StartTimer()
			}
		})
	}
}
