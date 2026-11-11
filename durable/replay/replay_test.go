package replay_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/replay"
	"github.com/urmzd/saige/durable/run"
)

func acceptRun(t *testing.T, lgr ledger.Ledger, id string) {
	t.Helper()
	if err := lgr.Accept(context.Background(), run.Run{ID: id, Name: "wf", Status: run.StatusPending}); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func frameFor(t *testing.T, lgr ledger.Ledger, id string, attempt int, deltas queue.DeltaLog) *replay.Frame {
	t.Helper()
	_, records, err := lgr.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return replay.NewFrame(id, attempt, lgr, run.JSONCodec{}, records, deltas)
}

func TestDo_MemoizesByNameOccurrence(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	calls := 0
	exec := func(f *replay.Frame) []string {
		var outs []string
		for i := 0; i < 3; i++ {
			out, err := f.Do(ctx, run.KindActivity, "work", "", []byte(`"in"`), func(context.Context) ([]byte, error) {
				calls++
				return []byte(fmt.Sprintf("%d", calls)), nil
			})
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			outs = append(outs, string(out))
		}
		return outs
	}

	first := exec(frameFor(t, lgr, "r1", 1, nil))
	if calls != 3 {
		t.Fatalf("first attempt executed %d times, want 3", calls)
	}
	second := exec(frameFor(t, lgr, "r1", 2, nil))
	if calls != 3 {
		t.Fatalf("replay re-executed: %d calls, want 3", calls)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("occurrence %d: replay %q != original %q", i, second[i], first[i])
		}
	}
}

func TestDo_InputDivergenceErrorsLoudly(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f1 := frameFor(t, lgr, "r1", 1, nil)
	if _, err := f1.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return []byte(`"x"`), nil
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	f2 := frameFor(t, lgr, "r1", 2, nil)
	_, err := f2.Do(ctx, run.KindActivity, "a", "", []byte(`2`), func(context.Context) ([]byte, error) {
		return []byte(`"y"`), nil
	})
	if !errors.Is(err, run.ErrNonDeterministic) {
		t.Fatalf("want ErrNonDeterministic, got %v", err)
	}
}

func TestDo_KindDivergenceErrorsLoudly(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f1 := frameFor(t, lgr, "r1", 1, nil)
	if _, err := f1.Do(ctx, run.KindStep, "now", "", nil, func(context.Context) ([]byte, error) {
		return []byte(`"t0"`), nil
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	f2 := frameFor(t, lgr, "r1", 2, nil)
	_, err := f2.Do(ctx, run.KindActivity, "now", "", nil, func(context.Context) ([]byte, error) {
		return []byte(`"t1"`), nil
	})
	if !errors.Is(err, run.ErrNonDeterministic) {
		t.Fatalf("want ErrNonDeterministic, got %v", err)
	}
}

func TestDo_RollingForward_InsertedAndRemovedCalls(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	// "Release 1": a then b.
	f1 := frameFor(t, lgr, "r1", 1, nil)
	mustDo(t, f1, ctx, "a", "va")
	mustDo(t, f1, ctx, "b", "vb")

	// "Release 2" replays with c inserted between: a memoized, c fresh, b memoized.
	execs := 0
	f2 := frameFor(t, lgr, "r1", 2, nil)
	got := map[string]string{}
	for _, name := range []string{"a", "c", "b"} {
		name := name
		out, err := f2.Do(ctx, run.KindActivity, name, "", nil2json(t, name), func(context.Context) ([]byte, error) {
			execs++
			return []byte(`"v` + name + `"`), nil
		})
		if err != nil {
			t.Fatalf("do %s: %v", name, err)
		}
		got[name] = string(out)
	}
	if execs != 1 {
		t.Fatalf("inserted-call replay executed %d, want 1 (only c)", execs)
	}
	if got["a"] != `"va"` || got["b"] != `"vb"` || got["c"] != `"vc"` {
		t.Fatalf("unexpected outputs: %v", got)
	}

	// "Release 3" removes b: completes fine, b is merely unconsumed.
	f3 := frameFor(t, lgr, "r1", 3, nil)
	mustDo(t, f3, ctx, "a", "va")
	mustDo(t, f3, ctx, "c", "vc")
	unconsumed := f3.Unconsumed()
	if len(unconsumed) != 1 || unconsumed[0] != "b#0" {
		t.Fatalf("unconsumed = %v, want [b#0]", unconsumed)
	}
}

func nil2json(t *testing.T, name string) []byte {
	t.Helper()
	return []byte(`"` + name + `-input"`)
}

func mustDo(t *testing.T, f *replay.Frame, ctx context.Context, name, val string) {
	t.Helper()
	if _, err := f.Do(ctx, run.KindActivity, name, "", nil2json(t, name), func(context.Context) ([]byte, error) {
		return []byte(`"` + val + `"`), nil
	}); err != nil {
		t.Fatalf("do %s: %v", name, err)
	}
}

func TestDo_UserKeysAreOrderIndependent(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f1 := frameFor(t, lgr, "r1", 1, nil)
	if _, err := f1.Do(ctx, run.KindActivity, "fan", "item-2", []byte(`2`), func(context.Context) ([]byte, error) {
		return []byte(`"two"`), nil
	}); err != nil {
		t.Fatalf("keyed: %v", err)
	}

	// Replay reaches the same logical call in a different order position.
	f2 := frameFor(t, lgr, "r1", 2, nil)
	if _, err := f2.Do(ctx, run.KindActivity, "other", "", []byte(`0`), func(context.Context) ([]byte, error) {
		return []byte(`"o"`), nil
	}); err != nil {
		t.Fatalf("other: %v", err)
	}
	out, err := f2.Do(ctx, run.KindActivity, "fan", "item-2", []byte(`2`), func(context.Context) ([]byte, error) {
		t.Fatal("keyed call re-executed on replay")
		return nil, nil
	})
	if err != nil || string(out) != `"two"` {
		t.Fatalf("keyed replay: %q, %v", out, err)
	}
}

func TestDo_RetryableFailureRecordsNothing(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f1 := frameFor(t, lgr, "r1", 1, nil)
	boom := errors.New("transient")
	if _, err := f1.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("want transient error, got %v", err)
	}
	_, records, _ := lgr.Load(ctx, "r1")
	if len(records) != 0 {
		t.Fatalf("retryable failure wrote %d records, want 0", len(records))
	}

	// Next attempt re-executes for real.
	f2 := frameFor(t, lgr, "r1", 2, nil)
	out, err := f2.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return []byte(`"ok"`), nil
	})
	if err != nil || string(out) != `"ok"` {
		t.Fatalf("retry: %q, %v", out, err)
	}
}

func TestDo_NonRetryableFailureIsMemoized(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f1 := frameFor(t, lgr, "r1", 1, nil)
	_, err := f1.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return nil, run.NonRetryable(errors.New("card declined"))
	})
	if !run.IsNonRetryable(err) {
		t.Fatalf("want non-retryable, got %v", err)
	}

	executed := false
	f2 := frameFor(t, lgr, "r1", 2, nil)
	_, err = f2.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		executed = true
		return []byte(`"nope"`), nil
	})
	var rec *run.RecordedError
	if !errors.As(err, &rec) {
		t.Fatalf("want RecordedError, got %v", err)
	}
	if executed {
		t.Fatal("memoized failure re-executed")
	}
	if !run.IsNonRetryable(err) {
		t.Fatal("RecordedError must be non-retryable")
	}
}

func TestDo_EventAbsentParks_PresentReturns(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f1 := frameFor(t, lgr, "r1", 1, nil)
	_, err := f1.Do(ctx, run.KindEvent, "approval", "", nil, nil)
	if !errors.Is(err, run.ErrParked) {
		t.Fatalf("want ErrParked, got %v", err)
	}

	// Anything writes the event record — a Signal, a SQL insert.
	if _, err := lgr.Record(ctx, run.Record{
		RunID: "r1", Key: "event:approval#0", Kind: run.KindEvent, Name: "approval",
		Status: run.RecordOK, Output: []byte(`"yes"`),
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}

	f2 := frameFor(t, lgr, "r1", 2, nil)
	out, err := f2.Do(ctx, run.KindEvent, "approval", "", nil, nil)
	if err != nil || string(out) != `"yes"` {
		t.Fatalf("event replay: %q, %v", out, err)
	}
}

func TestDo_ConcurrentAttemptsConvergeOnFirstWrite(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	// Attempt 2 records first; attempt 1 (a zombie) then executes the same
	// frontier call with a different result and must adopt the stored one.
	f2 := frameFor(t, lgr, "r1", 2, nil)
	if _, err := f2.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return []byte(`"winner"`), nil
	}); err != nil {
		t.Fatalf("attempt2: %v", err)
	}

	f1 := replay.NewFrame("r1", 1, lgr, run.JSONCodec{}, nil, nil) // stale prefetch: empty
	out, err := f1.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return []byte(`"loser"`), nil
	})
	if err != nil || string(out) != `"winner"` {
		t.Fatalf("zombie must adopt stored truth: %q, %v", out, err)
	}
}

func TestEmitAndDividers(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	acceptRun(t, lgr, "r1")

	f := frameFor(t, lgr, "r1", 1, q)
	if _, err := f.Do(ctx, run.KindActivity, "stream", "", []byte(`1`), func(c context.Context) ([]byte, error) {
		if err := f.Emit(c, []byte(`tok`)); err != nil {
			return nil, err
		}
		return []byte(`"done"`), nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	deltas, _, err := q.Read(ctx, "r1", "", 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var user, seal int
	for _, d := range deltas {
		switch d.Kind {
		case run.DeltaUser:
			user++
			if d.RecordKey != "stream#0" || d.Attempt != 1 {
				t.Fatalf("user delta mis-tagged: %+v", d)
			}
		case run.DeltaSeal:
			seal++
			if d.RecordKey != "stream#0" {
				t.Fatalf("seal mis-tagged: %+v", d)
			}
		}
	}
	if user != 1 || seal != 1 {
		t.Fatalf("user=%d seal=%d, want 1/1", user, seal)
	}

	// Replay: the memoized activity's fn never runs, so nothing re-streams.
	f2 := frameFor(t, lgr, "r1", 2, q)
	if _, err := f2.Do(ctx, run.KindActivity, "stream", "", []byte(`1`), func(c context.Context) ([]byte, error) {
		t.Fatal("memoized activity re-executed")
		return nil, nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	after, _, _ := q.Read(ctx, "r1", "", 100)
	for _, d := range after {
		if d.Kind == run.DeltaUser && d.Attempt == 2 {
			t.Fatalf("memoized activity emitted on replay: %+v", d)
		}
	}
}

func TestPriorDeltas(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	acceptRun(t, lgr, "r1")

	// Attempt 1 emits two tokens then "crashes" (no record written).
	f1 := frameFor(t, lgr, "r1", 1, q)
	_, _ = f1.Do(ctx, run.KindActivity, "llm", "", []byte(`1`), func(c context.Context) ([]byte, error) {
		_ = f1.Emit(c, []byte(`t1`))
		_ = f1.Emit(c, []byte(`t2`))
		return nil, errors.New("crash")
	})

	// Attempt 2 reads its prior segment.
	f2 := frameFor(t, lgr, "r1", 2, q)
	var prior [][]byte
	if _, err := f2.Do(ctx, run.KindActivity, "llm", "", []byte(`1`), func(c context.Context) ([]byte, error) {
		var perr error
		prior, perr = f2.PriorDeltas(c)
		if perr != nil {
			return nil, perr
		}
		return []byte(`"resumed"`), nil
	}); err != nil {
		t.Fatalf("attempt2: %v", err)
	}
	if len(prior) != 2 || string(prior[0]) != "t1" || string(prior[1]) != "t2" {
		t.Fatalf("prior deltas = %v", prior)
	}
}

// Regression: review finding — a memoized composite must not desynchronize
// occurrence counters. Keys are parent-scoped, so a parent whose body never
// re-executes on replay leaves top-level counters exactly where the live
// execution would have.
func TestDo_NestedCompositeKeysAreParentScoped(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	childCalls := 0
	runWorkflow := func(f *replay.Frame) (string, error) {
		child := func(c context.Context) ([]byte, error) {
			childCalls++
			return []byte(fmt.Sprintf("%d", childCalls)), nil
		}
		// Composite: an activity that itself calls the same child twice.
		if _, err := f.Do(ctx, run.KindActivity, "parent", "", []byte(`1`), func(c context.Context) ([]byte, error) {
			if _, err := f.Do(c, run.KindActivity, "child", "", []byte(`1`), child); err != nil {
				return nil, err
			}
			if _, err := f.Do(c, run.KindActivity, "child", "", []byte(`2`), child); err != nil {
				return nil, err
			}
			return []byte(`"composite"`), nil
		}); err != nil {
			return "", err
		}
		// Then the same child name at top level.
		out, err := f.Do(ctx, run.KindActivity, "child", "", []byte(`3`), child)
		return string(out), err
	}

	first, err := runWorkflow(frameFor(t, lgr, "r1", 1, nil))
	if err != nil {
		t.Fatalf("frontier: %v", err)
	}
	if childCalls != 3 {
		t.Fatalf("frontier child calls = %d, want 3", childCalls)
	}

	// Replay: parent memo-hits (its body, and therefore its nested child
	// calls, never run). The top-level child must still land on ITS record.
	second, err := runWorkflow(frameFor(t, lgr, "r1", 2, nil))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if childCalls != 3 {
		t.Fatalf("replay re-executed: child calls = %d, want 3", childCalls)
	}
	if first != second {
		t.Fatalf("top-level child diverged on replay: %q vs %q", first, second)
	}

	_, records, _ := lgr.Load(ctx, "r1")
	keys := map[string]bool{}
	for _, rec := range records {
		keys[rec.Key] = true
	}
	for _, want := range []string{"parent#0", "parent#0/child#0", "parent#0/child#1", "child#0"} {
		if !keys[want] {
			t.Fatalf("missing record key %q in %v", want, keys)
		}
	}
}

// Regression: review finding — a repeated explicit key within one live
// attempt must replay the just-written record, not re-execute the effect.
func TestDo_RepeatedUserKeyMemoizesWithinAttempt(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f := frameFor(t, lgr, "r1", 1, nil)
	calls := 0
	for i := 0; i < 2; i++ {
		out, err := f.Do(ctx, run.KindActivity, "fan", "item-7", []byte(`7`), func(context.Context) ([]byte, error) {
			calls++
			return []byte(`"once"`), nil
		})
		if err != nil || string(out) != `"once"` {
			t.Fatalf("call %d: %q, %v", i, out, err)
		}
	}
	if calls != 1 {
		t.Fatalf("repeated user key executed %d times in one attempt, want 1", calls)
	}
}

// Regression: review finding — a recorded non-retryable failure must surface
// identically on EVERY attempt, including the frontier one that recorded it.
// Determinism outranks error-identity fidelity.
func TestDo_NonRetryableErrorIsRecordedFormOnFrontier(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f := frameFor(t, lgr, "r1", 1, nil)
	_, err := f.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return nil, run.NonRetryable(errors.New("card declined"))
	})
	var rec *run.RecordedError
	if !errors.As(err, &rec) {
		t.Fatalf("frontier error = %T %v, want *run.RecordedError", err, err)
	}

	f2 := frameFor(t, lgr, "r1", 2, nil)
	_, err2 := f2.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		t.Fatal("memoized failure re-executed")
		return nil, nil
	})
	var rec2 *run.RecordedError
	if !errors.As(err2, &rec2) {
		t.Fatalf("replay error = %T, want *run.RecordedError", err2)
	}
	if rec.Message != rec2.Message || rec.Key != rec2.Key {
		t.Fatalf("error diverged across attempts: %+v vs %+v", rec, rec2)
	}
}

// Regression: review finding — the recorded codec is verified on replay; a
// mismatch is terminal, not retried.
func TestDo_CodecMismatchIsTerminal(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f := frameFor(t, lgr, "r1", 1, nil)
	if _, err := f.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return []byte(`"v"`), nil
	}); err != nil {
		t.Fatal(err)
	}

	_, records, _ := lgr.Load(ctx, "r1")
	if records[0].Codec != (run.JSONCodec{}).ContentType() {
		t.Fatalf("record codec = %q, want %q", records[0].Codec, (run.JSONCodec{}).ContentType())
	}

	f2 := replay.NewFrame("r1", 2, lgr, altCodec{}, records, nil)
	_, err := f2.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(context.Context) ([]byte, error) {
		return []byte(`"v"`), nil
	})
	if !run.IsNonRetryable(err) {
		t.Fatalf("codec mismatch error = %v, want non-retryable", err)
	}
}

// altCodec is JSON with a different ContentType, standing in for a
// misconfigured fleet.
type altCodec struct{ run.JSONCodec }

func (altCodec) ContentType() string { return "application/x-alt" }

// Regression: three-level nesting must not double ancestor segments in
// record keys (the innermost active key already embeds its full ancestry).
func TestDo_GrandchildKeysDoNotDoublePrefix(t *testing.T) {
	ctx := context.Background()
	lgr := ledger.NewMemory()
	acceptRun(t, lgr, "r1")

	f := frameFor(t, lgr, "r1", 1, nil)
	_, err := f.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(c context.Context) ([]byte, error) {
		return f.Do(c, run.KindActivity, "b", "", []byte(`1`), func(c context.Context) ([]byte, error) {
			return f.Do(c, run.KindActivity, "c", "", []byte(`1`), func(context.Context) ([]byte, error) {
				return []byte(`"leaf"`), nil
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, records, _ := lgr.Load(ctx, "r1")
	keys := map[string]bool{}
	for _, rec := range records {
		keys[rec.Key] = true
	}
	for _, want := range []string{"a#0", "a#0/b#0", "a#0/b#0/c#0"} {
		if !keys[want] {
			t.Fatalf("missing key %q; got %v", want, keys)
		}
	}

	// Replay must find every key again (no drift at any depth).
	f2 := frameFor(t, lgr, "r1", 2, nil)
	out, err := f2.Do(ctx, run.KindActivity, "a", "", []byte(`1`), func(c context.Context) ([]byte, error) {
		t.Fatal("memoized composite re-executed")
		return nil, nil
	})
	if err != nil || string(out) != `"leaf"` {
		t.Fatalf("replay: %q, %v", out, err)
	}
}
