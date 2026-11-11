package durable_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/replay"
	"github.com/urmzd/saige/durable/run"
)

// inRun executes fn under a fresh frame for the given run — the SDK-level
// harness for replay semantics without a worker.
func inRun(t *testing.T, lgr ledger.Ledger, dl queue.DeltaLog, runID string, attempt int, fn func(ctx context.Context) error) error {
	t.Helper()
	_, records, err := lgr.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	f := replay.NewFrame(runID, attempt, lgr, run.JSONCodec{}, records, dl)
	return fn(replay.WithFrame(context.Background(), f))
}

func newRun(t *testing.T, lgr ledger.Ledger, id string) {
	t.Helper()
	if err := lgr.Accept(context.Background(), run.Run{ID: id, Name: "wf", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestActivity_PassThroughOutsideRun(t *testing.T) {
	reg := run.NewRegistry()
	calls := 0
	double := durable.ActivityIn(reg, "double", func(_ context.Context, n int) (int, error) {
		calls++
		return n * 2, nil
	})
	out, err := double.Call(context.Background(), 21)
	if err != nil || out != 42 || calls != 1 {
		t.Fatalf("pass-through: out=%d err=%v calls=%d", out, err, calls)
	}
	if durable.InRun(context.Background()) {
		t.Fatal("InRun outside a run")
	}
	if k := durable.IdempotencyKey(context.Background()); k != "" {
		t.Fatalf("IdempotencyKey outside run = %q", k)
	}
}

func TestActivity_MemoizedAcrossAttempts(t *testing.T) {
	reg := run.NewRegistry()
	lgr := ledger.NewMemory()
	newRun(t, lgr, "r1")

	calls := 0
	stamp := durable.ActivityIn(reg, "stamp", func(_ context.Context, s string) (string, error) {
		calls++
		return s + "!", nil
	})

	var first, second string
	if err := inRun(t, lgr, nil, "r1", 1, func(ctx context.Context) error {
		var err error
		first, err = stamp.Call(ctx, "hey")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := inRun(t, lgr, nil, "r1", 2, func(ctx context.Context) error {
		var err error
		second, err = stamp.Call(ctx, "hey")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first != "hey!" || second != "hey!" {
		t.Fatalf("calls=%d first=%q second=%q", calls, first, second)
	}
}

func TestStep_FirstValueIsForever(t *testing.T) {
	lgr := ledger.NewMemory()
	newRun(t, lgr, "r1")

	n := 0
	roll := func(ctx context.Context) (int, error) {
		n++
		return n * 100, nil
	}
	var v1, v2 int
	_ = inRun(t, lgr, nil, "r1", 1, func(ctx context.Context) error {
		var err error
		v1, err = durable.Step(ctx, "roll", roll)
		return err
	})
	_ = inRun(t, lgr, nil, "r1", 2, func(ctx context.Context) error {
		var err error
		v2, err = durable.Step(ctx, "roll", roll)
		return err
	})
	if v1 != 100 || v2 != 100 || n != 1 {
		t.Fatalf("v1=%d v2=%d n=%d", v1, v2, n)
	}
}

func TestEvent_ParksThenResumesViaSignal(t *testing.T) {
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	c := durable.New(lgr, q)
	newRun(t, lgr, "r1")

	workflow := func(ctx context.Context) (string, error) {
		return durable.Event[string](ctx, "approval")
	}

	err := inRun(t, lgr, q, "r1", 1, func(ctx context.Context) error {
		_, err := workflow(ctx)
		return err
	})
	if !errors.Is(err, run.ErrParked) {
		t.Fatalf("want park, got %v", err)
	}

	if err := c.Signal(context.Background(), "r1", "approval", "granted"); err != nil {
		t.Fatalf("signal: %v", err)
	}

	var got string
	if err := inRun(t, lgr, q, "r1", 2, func(ctx context.Context) error {
		var err error
		got, err = workflow(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != "granted" {
		t.Fatalf("event value = %q", got)
	}

	// The signal also re-enqueued the run.
	claimCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	it, _, err := q.Claim(claimCtx, time.Second)
	if err != nil || it.RunID != "r1" {
		t.Fatalf("signal did not enqueue: %v %v", it, err)
	}
}

func TestSignal_SuccessiveOccurrences(t *testing.T) {
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	c := durable.New(lgr, q)
	newRun(t, lgr, "r1")

	if err := c.Signal(context.Background(), "r1", "input", "one"); err != nil {
		t.Fatal(err)
	}
	if err := c.Signal(context.Background(), "r1", "input", "two"); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := inRun(t, lgr, q, "r1", 1, func(ctx context.Context) error {
		for i := 0; i < 2; i++ {
			v, err := durable.Event[string](ctx, "input")
			if err != nil {
				return err
			}
			got = append(got, v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("events = %v", got)
	}
}

func TestStart_RunIDIsIdempotencyKey(t *testing.T) {
	reg := run.NewRegistry()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	c := durable.New(lgr, q)

	wf := durable.ActivityIn(reg, "wf", func(_ context.Context, s string) (string, error) { return s, nil })

	h1, err := durable.Start(context.Background(), c, wf, "a", durable.WithRunID("order-1"))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := durable.Start(context.Background(), c, wf, "DIFFERENT", durable.WithRunID("order-1"))
	if err != nil {
		t.Fatal(err)
	}
	if h1.RunID() != h2.RunID() {
		t.Fatalf("handles diverge: %s vs %s", h1.RunID(), h2.RunID())
	}
	r, _, err := lgr.Load(context.Background(), "order-1")
	if err != nil {
		t.Fatal(err)
	}
	var in string
	_ = (run.JSONCodec{}).Unmarshal(r.Input, &in)
	if in != "a" {
		t.Fatalf("resubmission mutated the run: input=%q", in)
	}
}

func TestStart_UnserializableInputFailsEagerly(t *testing.T) {
	reg := run.NewRegistry()
	c := durable.New(ledger.NewMemory(), queue.NewMemory())
	bad := durable.ActivityIn(reg, "bad", func(_ context.Context, ch chan int) (int, error) { return 0, nil })
	if _, err := durable.Start(context.Background(), c, bad, make(chan int)); err == nil {
		t.Fatal("chan input must fail at Start")
	}
}

func TestEmit_NoOpWithoutRunOrDeltaLog(t *testing.T) {
	if err := durable.Emit(context.Background(), "ignored"); err != nil {
		t.Fatalf("emit outside run: %v", err)
	}
	lgr := ledger.NewMemory()
	newRun(t, lgr, "r1")
	if err := inRun(t, lgr, nil, "r1", 1, func(ctx context.Context) error {
		return durable.Emit(ctx, "no delta log")
	}); err != nil {
		t.Fatalf("emit without capability: %v", err)
	}
}

func TestIdempotencyKey_InsideActivity(t *testing.T) {
	reg := run.NewRegistry()
	lgr := ledger.NewMemory()
	newRun(t, lgr, "r1")

	var key string
	capture := durable.ActivityIn(reg, "capture", func(ctx context.Context, _ durable.Void) (durable.Void, error) {
		key = durable.IdempotencyKey(ctx)
		return durable.Void{}, nil
	})
	if err := inRun(t, lgr, nil, "r1", 1, func(ctx context.Context) error {
		_, err := capture.Call(ctx, durable.Void{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if key != "r1:capture#0" {
		t.Fatalf("idempotency key = %q, want r1:capture#0", key)
	}
}

func TestDecodeDrift_IsTerminal(t *testing.T) {
	reg := run.NewRegistry()
	lgr := ledger.NewMemory()
	newRun(t, lgr, "r1")

	// A record whose payload no longer decodes into today's output type.
	if _, err := lgr.Record(context.Background(), run.Record{
		RunID: "r1", Key: "typed#0", Kind: run.KindActivity, Name: "typed",
		InputHash: run.HashInput([]byte(`"in"`)), Status: run.RecordOK,
		Output: []byte(`"a string, not an int"`),
	}); err != nil {
		t.Fatal(err)
	}
	typed := durable.ActivityIn(reg, "typed", func(_ context.Context, s string) (int, error) { return 1, nil })
	err := inRun(t, lgr, nil, "r1", 1, func(ctx context.Context) error {
		_, err := typed.Call(ctx, "in")
		return err
	})
	var de *durable.DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("want DecodeError, got %v", err)
	}
	if !run.IsNonRetryable(err) {
		t.Fatal("decode drift must be terminal")
	}
}
