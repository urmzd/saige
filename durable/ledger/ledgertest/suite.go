// Package ledgertest is the executable contract for ledger.Ledger: a
// conformance suite every implementation must pass. Adapters wire their
// constructor into Run from their own tests:
//
//	func TestMyLedger(t *testing.T) {
//		ledgertest.Run(t, func(t *testing.T) ledger.Ledger {
//			return openMyLedger(t) // fresh, empty ledger per subtest
//		})
//	}
//
// The factory is called once per subtest and must return an empty ledger;
// register any teardown on the provided *testing.T. Optional capabilities
// the ledger implements (ledger.PrefixReader) are exercised by their own
// subtests, discovered by type assertion; a ledger without one skips it.
package ledgertest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
)

// racers is the number of goroutines the concurrency subtests race against
// each other.
const racers = 16

// Run exercises the full Ledger contract against implementations produced by
// factory. Every subtest gets its own fresh ledger.
func Run(t *testing.T, factory func(t *testing.T) ledger.Ledger) {
	t.Run("accept is idempotent", func(t *testing.T) {
		testAcceptIdempotent(t, factory(t))
	})
	t.Run("complete is a one-shot CAS", func(t *testing.T) {
		testCompleteCAS(t, factory(t))
	})
	t.Run("first terminal write wins under concurrency", func(t *testing.T) {
		testCompleteFirstWriteWins(t, factory(t))
	})
	t.Run("record is write-once with first-write-wins adoption", func(t *testing.T) {
		testRecordWriteOnce(t, factory(t))
	})
	t.Run("record converges under concurrency", func(t *testing.T) {
		testRecordConvergence(t, factory(t))
	})
	t.Run("load returns the run and all records", func(t *testing.T) {
		testLoad(t, factory(t))
	})
	t.Run("list pending filters, cuts off, limits, and orders", func(t *testing.T) {
		testListPending(t, factory(t))
	})
	t.Run("mutation isolation", func(t *testing.T) {
		testMutationIsolation(t, factory(t))
	})
	t.Run("records with prefix", func(t *testing.T) {
		l := factory(t)
		pr, ok := l.(ledger.PrefixReader)
		if !ok {
			t.Skip("ledger does not implement the optional ledger.PrefixReader")
		}
		testRecordsWithPrefix(t, l, pr)
	})
}

func testAcceptIdempotent(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	orig := run.Run{
		ID:        "r-accept",
		Name:      "wf",
		Input:     []byte("original input"),
		Status:    run.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := l.Accept(ctx, orig); err != nil {
		t.Fatalf("first Accept: %v", err)
	}

	dup := orig
	dup.Name = "impostor"
	dup.Input = []byte("different input")
	if err := l.Accept(ctx, dup); err != nil {
		t.Fatalf("second Accept: got %v, want nil (idempotent no-op)", err)
	}

	got, _, err := l.Load(ctx, orig.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != orig.Name || !bytes.Equal(got.Input, orig.Input) {
		t.Errorf("second Accept overwrote the run: got Name=%q Input=%q, want Name=%q Input=%q",
			got.Name, got.Input, orig.Name, orig.Input)
	}
}

func testCompleteCAS(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	acceptPending(t, l, "r-cas", time.Now())

	first := run.Result{Status: run.StatusSucceeded, Output: []byte("done")}
	if err := l.Complete(ctx, "r-cas", 1, first); err != nil {
		t.Fatalf("first Complete: %v", err)
	}

	late := run.Result{Status: run.StatusFailed, Error: "too late"}
	if err := l.Complete(ctx, "r-cas", 2, late); !errors.Is(err, run.ErrAlreadyTerminal) {
		t.Errorf("second Complete: got %v, want run.ErrAlreadyTerminal", err)
	}

	got, _, err := l.Load(ctx, "r-cas")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != run.StatusSucceeded || !bytes.Equal(got.Output, first.Output) || got.Error != "" {
		t.Errorf("first terminal write did not stick: got Status=%q Output=%q Error=%q",
			got.Status, got.Output, got.Error)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1 (winning attempt, for audit)", got.Attempt)
	}
	if got.CompletedAt.IsZero() {
		t.Error("CompletedAt not set on Complete")
	}

	if err := l.Complete(ctx, "r-missing", 1, first); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Complete on unknown run: got %v, want run.ErrNotFound", err)
	}
}

func testCompleteFirstWriteWins(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	acceptPending(t, l, "r-race-complete", time.Now())

	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = l.Complete(ctx, "r-race-complete", i, run.Result{
				Status: run.StatusSucceeded,
				Output: fmt.Appendf(nil, "winner-%d", i),
			})
		}()
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("two Completes returned nil: goroutines %d and %d", winner, i)
			}
			winner = i
		case !errors.Is(err, run.ErrAlreadyTerminal):
			t.Errorf("goroutine %d: got %v, want run.ErrAlreadyTerminal", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no Complete returned nil; exactly one must win")
	}

	got, _, err := l.Load(ctx, "r-race-complete")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantOut := fmt.Appendf(nil, "winner-%d", winner)
	if !bytes.Equal(got.Output, wantOut) || got.Attempt != winner {
		t.Errorf("stored result is not the sole winner's: got Output=%q Attempt=%d, want Output=%q Attempt=%d",
			got.Output, got.Attempt, wantOut, winner)
	}
}

func testRecordWriteOnce(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	acceptPending(t, l, "r-rec", time.Now())

	first := run.Record{
		RunID:  "r-rec",
		Key:    "charge#0",
		Kind:   run.KindActivity,
		Name:   "charge",
		Status: run.RecordOK,
		Output: []byte("v1"),
	}
	stored, err := l.Record(ctx, first)
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if !bytes.Equal(stored.Output, first.Output) {
		t.Errorf("first Record returned Output=%q, want %q", stored.Output, first.Output)
	}
	if stored.RecordedAt.IsZero() {
		t.Error("RecordedAt not set on first write")
	}

	dup := first
	dup.Output = []byte("v2")
	got, err := l.Record(ctx, dup)
	if !errors.Is(err, run.ErrAlreadyRecorded) {
		t.Fatalf("duplicate Record: got %v, want run.ErrAlreadyRecorded", err)
	}
	if !bytes.Equal(got.Output, first.Output) {
		t.Errorf("duplicate Record returned Output=%q, want original %q (first write wins; caller adopts it)",
			got.Output, first.Output)
	}
}

func testRecordConvergence(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	acceptPending(t, l, "r-race-record", time.Now())

	stored := make([]run.Record, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stored[i], errs[i] = l.Record(ctx, run.Record{
				RunID:  "r-race-record",
				Key:    "step#0",
				Kind:   run.KindStep,
				Name:   "now",
				Status: run.RecordOK,
				Output: fmt.Appendf(nil, "v-%d", i),
			})
		}()
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("two Records returned nil: goroutines %d and %d", winner, i)
			}
			winner = i
		case !errors.Is(err, run.ErrAlreadyRecorded):
			t.Errorf("goroutine %d: got %v, want run.ErrAlreadyRecorded", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no Record returned nil; exactly one must win")
	}

	wantOut := fmt.Appendf(nil, "v-%d", winner)
	for i := range racers {
		if !bytes.Equal(stored[i].Output, wantOut) {
			t.Errorf("goroutine %d adopted Output=%q, want winner's %q (all callers must converge)",
				i, stored[i].Output, wantOut)
		}
	}
}

func testLoad(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	orig := run.Run{
		ID:        "r-load",
		Name:      "wf",
		Input:     []byte("payload"),
		Status:    run.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := l.Accept(ctx, orig); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	keys := []string{"charge#0", "step#0", "event:approved#0"}
	for _, key := range keys {
		if _, err := l.Record(ctx, run.Record{RunID: orig.ID, Key: key, Kind: run.KindActivity, Status: run.RecordOK}); err != nil {
			t.Fatalf("Record(%q): %v", key, err)
		}
	}

	got, recs, err := l.Load(ctx, orig.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != orig.ID || got.Name != orig.Name || !bytes.Equal(got.Input, orig.Input) {
		t.Errorf("Load run: got ID=%q Name=%q Input=%q, want ID=%q Name=%q Input=%q",
			got.ID, got.Name, got.Input, orig.ID, orig.Name, orig.Input)
	}
	gotKeys := make([]string, len(recs))
	for i, rec := range recs {
		gotKeys[i] = rec.Key
	}
	slices.Sort(gotKeys)
	wantKeys := slices.Clone(keys)
	slices.Sort(wantKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("Load records: got keys %v, want %v (any order)", gotKeys, wantKeys)
	}

	if _, _, err := l.Load(ctx, "r-missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Load on unknown run: got %v, want run.ErrNotFound", err)
	}
}

func testListPending(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cutoff := base.Add(10 * time.Minute)

	acceptPending(t, l, "old-a", base)
	acceptPending(t, l, "old-b", base.Add(1*time.Minute))
	acceptPending(t, l, "old-c", base.Add(2*time.Minute))
	acceptPending(t, l, "at-cutoff", cutoff) // CreatedAt == before: excluded (strictly before)
	acceptPending(t, l, "too-new", base.Add(time.Hour))
	acceptPending(t, l, "done", base)
	if err := l.Complete(ctx, "done", 1, run.Result{Status: run.StatusSucceeded}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := l.ListPending(ctx, cutoff, run.PendingCursor{}, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	want := []string{"old-a", "old-b", "old-c"}
	if !slices.Equal(pendingIDs(got), want) {
		t.Errorf("ListPending(cutoff, zero cursor, 10) = %v, want %v (terminal and at/after-cutoff runs excluded, ascending (CreatedAt, ID))",
			pendingIDs(got), want)
	}

	limited, err := l.ListPending(ctx, cutoff, run.PendingCursor{}, 2)
	if err != nil {
		t.Fatalf("ListPending with limit: %v", err)
	}
	if !slices.Equal(pendingIDs(limited), want[:2]) {
		t.Errorf("ListPending(cutoff, zero cursor, 2) = %v, want %v (oldest first, capped at limit)", pendingIDs(limited), want[:2])
	}

	// Cursor pagination: resuming strictly after the last row of a batch
	// walks the remainder without repeats or gaps. This is how a janitor
	// sweeps an arbitrarily large pending set — parked runs stay pending
	// forever, so a fixed first page must never be the whole story.
	last := limited[len(limited)-1]
	rest, err := l.ListPending(ctx, cutoff, run.PendingCursor{CreatedAt: last.CreatedAt, ID: last.ID}, 10)
	if err != nil {
		t.Fatalf("ListPending after cursor: %v", err)
	}
	if !slices.Equal(pendingIDs(rest), want[2:]) {
		t.Errorf("ListPending after cursor %v = %v, want %v", last, pendingIDs(rest), want[2:])
	}

	// Same-timestamp runs paginate by the ID tiebreak.
	tie := base.Add(5 * time.Minute)
	acceptPending(t, l, "tie-a", tie)
	acceptPending(t, l, "tie-b", tie)
	page1, err := l.ListPending(ctx, cutoff, run.PendingCursor{CreatedAt: tie, ID: ""}, 1)
	if err != nil {
		t.Fatalf("ListPending tie page 1: %v", err)
	}
	page2, err := l.ListPending(ctx, cutoff, run.PendingCursor{CreatedAt: tie, ID: "tie-a"}, 1)
	if err != nil {
		t.Fatalf("ListPending tie page 2: %v", err)
	}
	if !slices.Equal(pendingIDs(page1), []string{"tie-a"}) || !slices.Equal(pendingIDs(page2), []string{"tie-b"}) {
		t.Errorf("tie pagination = %v then %v, want [tie-a] then [tie-b]", pendingIDs(page1), pendingIDs(page2))
	}
}

// pendingIDs projects a ListPending batch to its run IDs.
func pendingIDs(batch []run.PendingRun) []string {
	ids := make([]string, len(batch))
	for i, p := range batch {
		ids[i] = p.ID
	}
	return ids
}

func testMutationIsolation(t *testing.T, l ledger.Ledger) {
	ctx := t.Context()
	input := []byte("input-0")
	if err := l.Accept(ctx, run.Run{ID: "r-iso", Name: "wf", Input: input, Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	input[0] = 'X' // caller keeps writing to its own buffer after Accept

	output := []byte("output-0")
	storedRec, err := l.Record(ctx, run.Record{RunID: "r-iso", Key: "s#0", Kind: run.KindStep, Status: run.RecordOK, Output: output})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	output[0] = 'X'           // caller's buffer
	storedRec.Output[0] = 'Y' // the copy Record handed back

	r1, recs1, err := l.Load(ctx, "r-iso")
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if !bytes.Equal(r1.Input, []byte("input-0")) {
		t.Errorf("Accept aliased the caller's Input: got %q, want %q", r1.Input, "input-0")
	}
	rec1 := findRecord(t, recs1, "s#0")
	if !bytes.Equal(rec1.Output, []byte("output-0")) {
		t.Errorf("Record aliased a caller buffer: got %q, want %q", rec1.Output, "output-0")
	}

	// Now deface everything the first Load returned.
	r1.Input[0] = 'Z'
	rec1.Output[0] = 'Z'

	r2, recs2, err := l.Load(ctx, "r-iso")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !bytes.Equal(r2.Input, []byte("input-0")) {
		t.Errorf("mutating a loaded run leaked into the store: got %q, want %q", r2.Input, "input-0")
	}
	rec2 := findRecord(t, recs2, "s#0")
	if !bytes.Equal(rec2.Output, []byte("output-0")) {
		t.Errorf("mutating a loaded record leaked into the store: got %q, want %q", rec2.Output, "output-0")
	}
}

func testRecordsWithPrefix(t *testing.T, l ledger.Ledger, pr ledger.PrefixReader) {
	ctx := t.Context()
	acceptPending(t, l, "r-prefix", time.Now())
	acceptPending(t, l, "r-neighbor", time.Now())
	acceptPending(t, l, "r-empty", time.Now())
	keys := []string{
		"event:approval#0", "event:approval#1", "event:approve#0", "Event:approval#0",
		"charge#0", "charge#0/refund#0", "100%_done#0", "100x_done#0", `back\slash#0`, "\u00e9moji#0",
	}
	for _, key := range keys {
		if _, err := l.Record(ctx, run.Record{RunID: "r-prefix", Key: key, Kind: run.KindStep, Status: run.RecordOK, Output: []byte(key)}); err != nil {
			t.Fatalf("Record(%q): %v", key, err)
		}
	}
	// Another run's matching record must never leak into r-prefix's read.
	if _, err := l.Record(ctx, run.Record{RunID: "r-neighbor", Key: "event:approval#9", Kind: run.KindEvent, Status: run.RecordOK}); err != nil {
		t.Fatalf("Record on the neighbor: %v", err)
	}

	read := func(runID string, prefixes ...string) []string {
		t.Helper()
		recs, err := pr.RecordsWithPrefix(ctx, runID, prefixes...)
		if err != nil {
			t.Fatalf("RecordsWithPrefix(%q, %q): %v", runID, prefixes, err)
		}
		got := make([]string, len(recs))
		for i, rec := range recs {
			if rec.RunID != runID || string(rec.Output) != rec.Key || rec.Status != run.RecordOK {
				t.Errorf("RecordsWithPrefix(%q, %q) returned %+v, want the stored record", runID, prefixes, rec)
			}
			got[i] = rec.Key
		}
		slices.Sort(got)
		return got
	}
	sorted := func(keys ...string) []string {
		out := slices.Clone(keys)
		slices.Sort(out)
		return out
	}

	for _, tc := range []struct {
		prefixes []string
		want     []string
	}{
		{[]string{"event:approval#"}, sorted("event:approval#0", "event:approval#1")},
		{[]string{"event:appro"}, sorted("event:approval#0", "event:approval#1", "event:approve#0")},
		// A record matching several prefixes comes back once.
		{[]string{"event:", "event:approval", "event:approval#1"}, sorted("event:approval#0", "event:approval#1", "event:approve#0")},
		{[]string{"charge#0", "nope"}, sorted("charge#0", "charge#0/refund#0")},
		// Prefixes are literal: LIKE's wildcards and escape match themselves.
		{[]string{"100%"}, sorted("100%_done#0")},
		{[]string{"100_"}, sorted()},
		{[]string{"100%_"}, sorted("100%_done#0")},
		{[]string{`back\`}, sorted(`back\slash#0`)},
		{[]string{"\u00e9mo"}, sorted("\u00e9moji#0")},
		{[]string{"EVENT:"}, sorted()},
		{[]string{""}, sorted(keys...)},
		{nil, sorted()},
	} {
		if got := read("r-prefix", tc.prefixes...); !slices.Equal(got, tc.want) {
			t.Errorf("RecordsWithPrefix(r-prefix, %q) keys = %q, want %q", tc.prefixes, got, tc.want)
		}
	}
	if got := read("r-empty", ""); len(got) != 0 {
		t.Errorf("RecordsWithPrefix on a run without records = %q, want none", got)
	}
	if _, err := pr.RecordsWithPrefix(ctx, "r-missing", "event:"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RecordsWithPrefix on an unknown run: got %v, want run.ErrNotFound", err)
	}

	// What comes back is the caller's to mutate.
	recs, err := pr.RecordsWithPrefix(ctx, "r-prefix", "charge#0/")
	if err != nil || len(recs) != 1 {
		t.Fatalf("RecordsWithPrefix(charge#0/) = %v, %v; want one record", recs, err)
	}
	recs[0].Output[0] = 'X'
	again, err := pr.RecordsWithPrefix(ctx, "r-prefix", "charge#0/")
	if err != nil || len(again) != 1 || string(again[0].Output) != "charge#0/refund#0" {
		t.Errorf("mutating a prefix read leaked into the store: %v, %v", again, err)
	}
}

// acceptPending registers a pending run with the given CreatedAt, failing the
// test on error.
func acceptPending(t *testing.T, l ledger.Ledger, id string, createdAt time.Time) {
	t.Helper()
	r := run.Run{ID: id, Name: "wf", Status: run.StatusPending, CreatedAt: createdAt}
	if err := l.Accept(t.Context(), r); err != nil {
		t.Fatalf("Accept(%q): %v", id, err)
	}
}

// findRecord returns the record with the given key, failing the test if it is
// absent. Load guarantees no order, so lookup is by key.
func findRecord(t *testing.T, recs []run.Record, key string) run.Record {
	t.Helper()
	for _, rec := range recs {
		if rec.Key == key {
			return rec
		}
	}
	t.Fatalf("record %q not found among %d records", key, len(recs))
	return run.Record{}
}
