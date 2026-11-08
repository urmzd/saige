package ledger_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/ledger/ledgertest"
	"github.com/urmzd/saige/durable/run"
)

func TestMemoryConformance(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) ledger.Ledger {
		return ledger.NewMemory()
	})
}

func TestMemoryRecordUnknownRun(t *testing.T) {
	m := ledger.NewMemory()
	_, err := m.Record(t.Context(), run.Record{RunID: "ghost", Key: "a#0", Kind: run.KindActivity, Status: run.RecordOK})
	if !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("Record against unknown run: got %v, want run.ErrNotFound", err)
	}
}

// A zombie attempt may still checkpoint after another attempt completed the
// run; Memory accepts the write and keeps the record log append-only.
func TestMemoryRecordAfterTerminalIsAllowed(t *testing.T) {
	ctx := t.Context()
	m := ledger.NewMemory()
	if err := m.Accept(ctx, run.Run{ID: "r1", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := m.Complete(ctx, "r1", 1, run.Result{Status: run.StatusSucceeded}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	stored, err := m.Record(ctx, run.Record{RunID: "r1", Key: "late#0", Kind: run.KindActivity, Status: run.RecordOK, Output: []byte("zombie")})
	if err != nil {
		t.Fatalf("Record after terminal: got %v, want nil (harmless zombie write)", err)
	}
	if !bytes.Equal(stored.Output, []byte("zombie")) {
		t.Errorf("stored Output = %q, want %q", stored.Output, "zombie")
	}

	_, recs, err := m.Load(ctx, "r1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 || recs[0].Key != "late#0" {
		t.Errorf("Load after zombie write: got %d records, want the one keyed %q", len(recs), "late#0")
	}
}

func TestMemoryRecordKeepsCallerRecordedAt(t *testing.T) {
	ctx := t.Context()
	m := ledger.NewMemory()
	if err := m.Accept(ctx, run.Run{ID: "r1", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	at := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	stored, err := m.Record(ctx, run.Record{RunID: "r1", Key: "a#0", Kind: run.KindStep, Status: run.RecordOK, RecordedAt: at})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !stored.RecordedAt.Equal(at) {
		t.Errorf("RecordedAt = %v, want caller-supplied %v preserved", stored.RecordedAt, at)
	}
}

// Memory documents limit <= 0 as "no limit".
func TestMemoryListPendingNoLimit(t *testing.T) {
	ctx := t.Context()
	m := ledger.NewMemory()
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		r := run.Run{ID: fmt.Sprintf("r-%d", i), Status: run.StatusPending, CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := m.Accept(ctx, r); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	for _, limit := range []int{0, -1} {
		got, err := m.ListPending(ctx, base.Add(time.Hour), run.PendingCursor{}, limit)
		if err != nil {
			t.Fatalf("ListPending(limit=%d): %v", limit, err)
		}
		if len(got) != 3 {
			t.Errorf("ListPending(limit=%d) returned %d IDs, want all 3 (limit <= 0 means no limit)", limit, len(got))
		}
	}
}

// Ties on CreatedAt order deterministically by ID.
func TestMemoryListPendingTieBreak(t *testing.T) {
	ctx := t.Context()
	m := ledger.NewMemory()
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"b", "c", "a"} {
		if err := m.Accept(ctx, run.Run{ID: id, Status: run.StatusPending, CreatedAt: at}); err != nil {
			t.Fatalf("Accept(%q): %v", id, err)
		}
	}

	got, err := m.ListPending(ctx, at.Add(time.Minute), run.PendingCursor{}, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("ListPending tie order = %v, want %v", got, want)
		}
	}
}

// Mixed operations racing across runs must stay consistent under -race.
func TestMemoryConcurrentMixedOps(t *testing.T) {
	ctx := t.Context()
	m := ledger.NewMemory()
	const workers = 8
	const recordsPerRun = 20

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("r-%d", i)
			r := run.Run{ID: id, Name: "wf", Status: run.StatusPending, CreatedAt: time.Now()}
			if err := m.Accept(ctx, r); err != nil {
				t.Errorf("Accept(%q): %v", id, err)
				return
			}
			for j := range recordsPerRun {
				rec := run.Record{RunID: id, Key: fmt.Sprintf("k#%d", j), Kind: run.KindStep, Status: run.RecordOK}
				if _, err := m.Record(ctx, rec); err != nil {
					t.Errorf("Record(%q, k#%d): %v", id, j, err)
				}
				if _, _, err := m.Load(ctx, id); err != nil {
					t.Errorf("Load(%q): %v", id, err)
				}
				if _, err := m.ListPending(ctx, time.Now(), run.PendingCursor{}, 100); err != nil {
					t.Errorf("ListPending: %v", err)
				}
			}
			if err := m.Complete(ctx, id, 1, run.Result{Status: run.StatusSucceeded}); err != nil {
				t.Errorf("Complete(%q): %v", id, err)
			}
		}()
	}
	wg.Wait()

	for i := range workers {
		id := fmt.Sprintf("r-%d", i)
		r, recs, err := m.Load(ctx, id)
		if err != nil {
			t.Fatalf("Load(%q): %v", id, err)
		}
		if r.Status != run.StatusSucceeded {
			t.Errorf("run %q: Status = %q, want %q", id, r.Status, run.StatusSucceeded)
		}
		if len(recs) != recordsPerRun {
			t.Errorf("run %q: %d records, want %d", id, len(recs), recordsPerRun)
		}
	}
}
