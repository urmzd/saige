package filewal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

var errInjectedWrite = errors.New("injected write failure")

// flakyFile wraps the WAL's real append handle and can fail one write after
// letting half the record's bytes reach the file — the mid-log-corruption
// hazard writeRecord's rollback exists to prevent.
type flakyFile struct {
	*os.File
	failNextWrite  bool  // next Write persists half the bytes, returns errInjectedWrite
	shortNextWrite bool  // next Write persists half the bytes, returns (n, nil)
	truncateErr    error // if set, Truncate fails with this error
}

func (f *flakyFile) Write(p []byte) (int, error) {
	if f.failNextWrite {
		f.failNextWrite = false
		n, _ := f.File.Write(p[:len(p)/2])
		return n, errInjectedWrite
	}
	if f.shortNextWrite {
		f.shortNextWrite = false
		return f.File.Write(p[:len(p)/2])
	}
	return f.File.Write(p)
}

func (f *flakyFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.File.Truncate(size)
}

func commitBranchTx(t *testing.T, w *WAL, tip string) types.TxID {
	t.Helper()
	ctx := context.Background()
	txID, err := w.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	op := types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: types.NodeID(tip)}
	if err := w.Append(ctx, txID, op); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return txID
}

// TestWriteRecordRollsBackPartialWrite proves a failed or short write cannot
// poison the middle of the log: the partial record is truncated away, a later
// commit appends cleanly, and a reopen reads only the good records.
func TestWriteRecordRollsBackPartialWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(ff *flakyFile)
	}{
		{"write error after partial bytes", func(ff *flakyFile) { ff.failNextWrite = true }},
		{"short write with nil error", func(ff *flakyFile) { ff.shortNextWrite = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "wal.jsonl")
			w, err := New(path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer w.Close()

			tx1 := commitBranchTx(t, w, "n1")

			ff := &flakyFile{File: w.f.(*os.File)}
			tc.arm(ff)
			w.f = ff

			txBad, err := w.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if err := w.Append(ctx, txBad, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "bad"}); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := w.Commit(ctx, txBad); err == nil {
				t.Fatal("Commit with injected write failure succeeded, want error")
			}

			// The very next commit must land on a clean record boundary.
			tx3 := commitBranchTx(t, w, "n3")

			w.Close()
			reopened, err := New(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer reopened.Close()

			committed, err := reopened.Recover(ctx)
			if err != nil {
				t.Fatalf("Recover after rolled-back partial write: %v", err)
			}
			if len(committed) != 2 || committed[0] != tx1 || committed[1] != tx3 {
				t.Fatalf("Recover = %v, want [%s %s]", committed, tx1, tx3)
			}
			ops, err := reopened.Replay(ctx, tx3)
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if len(ops) != 1 || ops[0].TipID != "n3" {
				t.Fatalf("Replay ops = %+v, want single set_branch to n3", ops)
			}
		})
	}
}

// TestWriteRecordFailedTruncateDisablesWAL proves that when the rollback
// truncate fails — leaving a partial record stuck at EOF — the WAL refuses
// further writes instead of appending after the corruption.
func TestWriteRecordFailedTruncateDisablesWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ff := &flakyFile{
		File:          w.f.(*os.File),
		failNextWrite: true,
		truncateErr:   errors.New("injected truncate failure"),
	}
	w.f = ff

	txBad, _ := w.Begin(ctx)
	_ = w.Append(ctx, txBad, types.TxOp{Kind: types.TxOpSetBranch, BranchID: "main", TipID: "bad"})
	if err := w.Commit(ctx, txBad); err == nil {
		t.Fatal("Commit with failed write+truncate succeeded, want error")
	}

	// Every subsequent write path must now error.
	tx2, _ := w.Begin(ctx)
	if err := w.Commit(ctx, tx2); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("Commit on failed WAL = %v, want disabled error", err)
	}
	if err := w.MarkApplied(ctx, "whatever"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("MarkApplied on failed WAL = %v, want disabled error", err)
	}
	if err := w.Compact(ctx); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("Compact on failed WAL = %v, want disabled error", err)
	}
}
