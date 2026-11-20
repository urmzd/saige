package fileledger_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/fileledger"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/ledger/ledgertest"
	"github.com/urmzd/saige/durable/run"
)

func newLedger(t *testing.T) *fileledger.Ledger {
	t.Helper()
	l, err := fileledger.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestConformance(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) ledger.Ledger { return newLedger(t) })
}

// Two ledgers on one directory are two processes: a record one writes the
// other reads, and the second write of a key adopts the first.
func TestTwoLedgersShareOneDirectory(t *testing.T) {
	dir := t.TempDir()
	a, _ := fileledger.New(dir)
	b, _ := fileledger.New(dir)
	ctx := context.Background()
	if err := a.Accept(ctx, run.Run{ID: "r1", Name: "w", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Record(ctx, run.Record{RunID: "r1", Key: "step#0", Kind: run.KindStep, Name: "step", Status: run.RecordOK, Output: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
	stored, err := b.Record(ctx, run.Record{RunID: "r1", Key: "step#0", Kind: run.KindStep, Name: "step", Status: run.RecordOK, Output: []byte(`2`)})
	if !errors.Is(err, run.ErrAlreadyRecorded) || string(stored.Output) != "1" {
		t.Fatalf("second writer got %q, %v; want the first record and ErrAlreadyRecorded", stored.Output, err)
	}
	r, recs, err := b.Load(ctx, "r1")
	if err != nil || r.Name != "w" || len(recs) != 1 {
		t.Fatalf("load = %+v, %d records, %v", r, len(recs), err)
	}
}

// Delete removes the run and its records and leaves other files in the run
// directory alone.
func TestDelete(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	if err := l.Accept(ctx, run.Run{ID: "r1", Name: "w", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Record(ctx, run.Record{RunID: "r1", Key: "a#0", Kind: run.KindStep, Name: "a", Status: run.RecordOK}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(fileledger.RunDir(l.Dir(), "r1"), "queue.json")
	if err := os.WriteFile(other, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Load(ctx, "r1"); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("load after delete: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("delete removed a file the ledger does not own: %v", err)
	}
	if err := l.Delete(ctx, "r1"); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// A temporary file left by a publish that crashed before its link is never
// read as a record.
func TestAnUnfinishedPublishIsNotARecord(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	if err := l.Accept(ctx, run.Run{ID: "r1", Name: "w", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(fileledger.RunDir(l.Dir(), "r1"), "records", ".tmp-123")
	if err := os.WriteFile(junk, []byte(`{"key":"half`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, recs, err := l.Load(ctx, "r1"); err != nil || len(recs) != 0 {
		t.Fatalf("load = %d records, %v", len(recs), err)
	}
}
