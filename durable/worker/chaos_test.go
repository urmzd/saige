package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

// TestReplay_CompletedPrefixNeverReruns kills a worker between the second
// and third checkpoint and proves the successor replays the recorded prefix
// without re-executing it: a and b run exactly once across both workers.
func TestReplay_CompletedPrefixNeverReruns(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	var aN, bN, cN atomic.Int64
	cGate := make(chan struct{})
	var cFirst atomic.Bool
	cFirst.Store(true)

	actA := durable.ActivityIn(reg, "a", func(_ context.Context, n int) (int, error) {
		aN.Add(1)
		return n + 1, nil
	})
	actB := durable.ActivityIn(reg, "b", func(_ context.Context, n int) (int, error) {
		bN.Add(1)
		return n * 2, nil
	})
	actC := durable.ActivityIn(reg, "c", func(ctx context.Context, n int) (int, error) {
		cN.Add(1)
		if cFirst.CompareAndSwap(true, false) {
			select {
			case <-cGate:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return n + 3, nil
	})
	wf := durable.ActivityIn(reg, "pipeline", func(ctx context.Context, n int) (int, error) {
		a, err := actA.Call(ctx, n)
		if err != nil {
			return 0, err
		}
		b, err := actB.Call(ctx, a)
		if err != nil {
			return 0, err
		}
		return actC.Call(ctx, b)
	})

	w1 := startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, 4)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitForRecords(t, lgr, h.RunID(), "a#0", "b#0")
	if aN.Load() != 1 || bN.Load() != 1 {
		t.Fatalf("prefix executions = %d/%d before kill, want 1/1", aN.Load(), bN.Load())
	}

	w1.stop()    // worker 1 dies with c in flight
	close(cGate) // and only then may c proceed

	startWorker(t, testWorker(lgr, q, reg))
	out, err := h.Result(resultCtx(t))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if want := (4+1)*2 + 3; out != want {
		t.Fatalf("output = %d, want %d", out, want)
	}
	if aN.Load() != 1 {
		t.Errorf("a executed %d times: a recorded checkpoint reran", aN.Load())
	}
	if bN.Load() != 1 {
		t.Errorf("b executed %d times: a recorded checkpoint reran", bN.Load())
	}
	if got := cN.Load(); got != 1 && got != 2 {
		t.Errorf("c executed %d times, want 1 or 2", got)
	}
}

// chaosOut is the chaos workflow's output: a deterministic activity chain
// plus one memoized step value.
type chaosOut struct {
	Sum  int `json:"sum"`
	Step int `json:"step"`
}

// TestChaos_RandomKills kills worker 1 at a seed-derived instant somewhere
// in a 4-activity+step run, lets worker 2 finish, and asserts the invariants
// that make durable anchor: exact output, exactly one record per key, at
// most one duplicate execution per activity, and a step value consistent
// with its single record.
func TestChaos_RandomKills(t *testing.T) {
	t.Parallel()
	const seeds = 100
	for seed := range seeds {
		t.Run(fmt.Sprintf("seed=%03d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(seed), 0xda7a))
			killAfter := time.Duration(rng.Int64N(16)) * time.Millisecond

			lgr := ledger.NewMemory()
			q := queue.NewMemory()
			reg := run.NewRegistry()

			var counts [4]atomic.Int64
			var stepSrc atomic.Int64

			mk := func(i int, name string, f func(int) int) *durable.ActivityFn[int, int] {
				return durable.ActivityIn(reg, name, func(ctx context.Context, n int) (int, error) {
					counts[i].Add(1)
					select {
					case <-time.After(2 * time.Millisecond):
					case <-ctx.Done():
						return 0, ctx.Err()
					}
					return f(n), nil
				})
			}
			a1 := mk(0, "a1", func(n int) int { return n*2 + 1 })
			a2 := mk(1, "a2", func(n int) int { return n + 7 })
			a3 := mk(2, "a3", func(n int) int { return n * 3 })
			a4 := mk(3, "a4", func(n int) int { return n - 5 })

			wf := durable.ActivityIn(reg, "chaos", func(ctx context.Context, n int) (chaosOut, error) {
				x, err := a1.Call(ctx, n)
				if err != nil {
					return chaosOut{}, err
				}
				if x, err = a2.Call(ctx, x); err != nil {
					return chaosOut{}, err
				}
				if x, err = a3.Call(ctx, x); err != nil {
					return chaosOut{}, err
				}
				if x, err = a4.Call(ctx, x); err != nil {
					return chaosOut{}, err
				}
				s, err := durable.Step(ctx, "pick", func(context.Context) (int, error) {
					return int(stepSrc.Add(1)) * 1000, nil
				})
				if err != nil {
					return chaosOut{}, err
				}
				return chaosOut{Sum: x, Step: s}, nil
			})

			chaosOpts := []worker.Option{
				worker.WithLeaseTTL(40 * time.Millisecond),
				worker.WithHeartbeatEvery(10 * time.Millisecond),
			}
			w1 := startWorker(t, testWorker(lgr, q, reg, chaosOpts...))
			c := durable.New(lgr, q)
			input := seed % 17
			h, err := durable.Start(context.Background(), c, wf, input)
			if err != nil {
				t.Fatalf("start: %v", err)
			}

			time.Sleep(killAfter)
			w1.stop()
			startWorker(t, testWorker(lgr, q, reg, chaosOpts...))

			rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer rcancel()
			out, err := h.Result(rctx)
			if err != nil {
				t.Fatalf("result (killed after %v): %v", killAfter, err)
			}

			if want := ((input*2+1)+7)*3 - 5; out.Sum != want {
				t.Fatalf("sum = %d, want %d", out.Sum, want)
			}

			_, records, err := lgr.Load(context.Background(), h.RunID())
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			expect := map[string]bool{"a1#0": true, "a2#0": true, "a3#0": true, "a4#0": true, "pick#0": true}
			var stepRec run.Record
			for _, rec := range records {
				if !expect[rec.Key] {
					t.Fatalf("unexpected record key %q", rec.Key)
				}
				if rec.Key == "pick#0" {
					stepRec = rec
				}
			}
			if len(records) != len(expect) {
				t.Fatalf("got %d records, want %d (exactly one per key)", len(records), len(expect))
			}

			var recorded int
			if err := json.Unmarshal(stepRec.Output, &recorded); err != nil {
				t.Fatalf("decode step record: %v", err)
			}
			if out.Step != recorded {
				t.Fatalf("output step = %d, recorded step = %d: output diverged from its record", out.Step, recorded)
			}

			for i := range counts {
				if n := counts[i].Load(); n < 1 || n > 2 {
					t.Fatalf("a%d executed %d times, want 1..2 (one crash window, at most one duplicate)", i+1, n)
				}
			}
		})
	}
}

// TestDeltas_DividerSplit kills a worker mid-stream and asserts the delta
// log's divider algebra: orphaned attempt-1 chunks sit split (not lost)
// between the attempt dividers, memoized activities never re-stream, and a
// seal lands only under the attempt that actually checkpointed the record.
func TestDeltas_DividerSplit(t *testing.T) {
	t.Parallel()
	lgr := ledger.NewMemory()
	q := queue.NewMemory()
	reg := run.NewRegistry()

	streamGate := make(chan struct{})
	var streamFirst atomic.Bool
	streamFirst.Store(true)

	before := durable.ActivityIn(reg, "before", func(ctx context.Context, s string) (string, error) {
		if err := durable.Emit(ctx, "prelude"); err != nil {
			return "", err
		}
		return s + "!", nil
	})
	stream := durable.ActivityIn(reg, "stream", func(ctx context.Context, s string) (string, error) {
		for i := range 3 {
			if err := durable.Emit(ctx, fmt.Sprintf("chunk-%d", i)); err != nil {
				return "", err
			}
		}
		if streamFirst.CompareAndSwap(true, false) {
			select {
			case <-streamGate:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return s + "$", nil
	})
	wf := durable.ActivityIn(reg, "streaming", func(ctx context.Context, s string) (string, error) {
		b, err := before.Call(ctx, s)
		if err != nil {
			return "", err
		}
		return stream.Call(ctx, b)
	})

	w1 := startWorker(t, testWorker(lgr, q, reg))
	c := durable.New(lgr, q)
	h, err := durable.Start(context.Background(), c, wf, "in")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Attempt 1 must die mid-stream: wait for its three chunks, then kill.
	waitForStreamDeltas(t, q, h.RunID(), 3)
	w1.stop()
	close(streamGate) // second execution never blocks

	startWorker(t, testWorker(lgr, q, reg))
	out, err := h.Result(resultCtx(t))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if out != "in!$" {
		t.Fatalf("output = %q, want %q", out, "in!$")
	}

	deltas, _, err := q.Read(context.Background(), h.RunID(), "", 0)
	if err != nil {
		t.Fatalf("read deltas: %v", err)
	}

	dividers := map[int]int{} // attempt → log index of its divider
	for i, d := range deltas {
		if d.Kind == run.DeltaAttempt {
			if _, dup := dividers[d.Attempt]; dup {
				t.Fatalf("duplicate attempt divider for attempt %d", d.Attempt)
			}
			dividers[d.Attempt] = i
		}
	}
	att1, ok1 := dividers[1]
	att2, ok2 := dividers[2]
	if !ok1 || !ok2 {
		t.Fatalf("missing attempt dividers: %v", dividers)
	}
	if len(dividers) != 2 || att1 >= att2 {
		t.Fatalf("dividers = %v, want exactly attempts 1 and 2 in order", dividers)
	}

	var beforeUser, beforeSeal, stream1, stream2, streamSeal int
	for i, d := range deltas {
		switch {
		case d.Kind == run.DeltaUser && d.RecordKey == "before#0":
			beforeUser++
			if d.Attempt != 1 || i < att1 || i > att2 {
				t.Fatalf("delta %d: %q streamed outside attempt 1's segment (memoized replay must not re-stream)", i, d.RecordKey)
			}
		case d.Kind == run.DeltaSeal && d.RecordKey == "before#0":
			beforeSeal++
			if d.Attempt != 1 || i > att2 {
				t.Fatalf("delta %d: seal for %q outside attempt 1's segment", i, d.RecordKey)
			}
		case d.Kind == run.DeltaUser && d.RecordKey == "stream#0":
			switch d.Attempt {
			case 1:
				stream1++
				if i < att1 || i > att2 {
					t.Fatalf("delta %d: orphaned attempt-1 chunk not between the dividers", i)
				}
			case 2:
				stream2++
				if i < att2 {
					t.Fatalf("delta %d: attempt-2 chunk before the attempt-2 divider", i)
				}
			default:
				t.Fatalf("delta %d: stream chunk from unexpected attempt %d", i, d.Attempt)
			}
		case d.Kind == run.DeltaSeal && d.RecordKey == "stream#0":
			streamSeal++
			if d.Attempt != 2 || i < att2 {
				t.Fatalf("delta %d: %q sealed under attempt %d, want only attempt 2", i, d.RecordKey, d.Attempt)
			}
		}
	}
	if beforeUser != 1 || beforeSeal != 1 {
		t.Fatalf("before: %d user deltas, %d seals; want 1 and 1", beforeUser, beforeSeal)
	}
	if stream1 != 3 || stream2 != 3 {
		t.Fatalf("stream chunks: attempt1=%d attempt2=%d, want 3 and 3 (orphaned but split, not lost)", stream1, stream2)
	}
	if streamSeal != 1 {
		t.Fatalf("stream seals = %d, want exactly 1 (under attempt 2)", streamSeal)
	}
}

// waitForStreamDeltas polls the delta log until the "stream#0" record key
// has emitted want user deltas.
func waitForStreamDeltas(t *testing.T, dl queue.DeltaLog, runID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		deltas, _, err := dl.Read(context.Background(), runID, "", 0)
		if err != nil {
			t.Fatalf("read deltas: %v", err)
		}
		n := 0
		for _, d := range deltas {
			if d.Kind == run.DeltaUser && d.RecordKey == "stream#0" {
				n++
			}
		}
		if n >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("stream deltas never reached %d for run %s", want, runID)
}

// TestRollingForward exercises code evolution against immutable records:
// inserted calls execute fresh, removed calls leave harmless leftovers, and
// changed inputs fail loudly.
func TestRollingForward(t *testing.T) {
	t.Parallel()

	t.Run("new activity added before the park point", func(t *testing.T) {
		t.Parallel()
		lgr := ledger.NewMemory()
		q := queue.NewMemory()

		var aN, cN atomic.Int64
		aFn := func(_ context.Context, s string) (string, error) {
			aN.Add(1)
			return "A(" + s + ")", nil
		}
		cFn := func(_ context.Context, s string) (string, error) {
			cN.Add(1)
			return "C(" + s + ")", nil
		}

		regV1 := run.NewRegistry()
		aV1 := durable.ActivityIn(regV1, "a", aFn)
		wfV1 := durable.ActivityIn(regV1, "roll", func(ctx context.Context, in string) (string, error) {
			av, err := aV1.Call(ctx, in)
			if err != nil {
				return "", err
			}
			ev, err := durable.Event[string](ctx, "go")
			if err != nil {
				return "", err
			}
			return av + "+" + ev, nil
		})

		w1 := startWorker(t, testWorker(lgr, q, regV1))
		c := durable.New(lgr, q)
		h, err := durable.Start(context.Background(), c, wfV1, "in", durable.WithMaxAttempts(1000))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		awaitParked(t, lgr, q, h.RunID(), 50*time.Millisecond)
		w1.stop()

		// v2 of the SAME workflow name inserts "c" before the event.
		regV2 := run.NewRegistry()
		aV2 := durable.ActivityIn(regV2, "a", aFn)
		cV2 := durable.ActivityIn(regV2, "c", cFn)
		durable.ActivityIn(regV2, "roll", func(ctx context.Context, in string) (string, error) {
			av, err := aV2.Call(ctx, in)
			if err != nil {
				return "", err
			}
			cv, err := cV2.Call(ctx, av)
			if err != nil {
				return "", err
			}
			ev, err := durable.Event[string](ctx, "go")
			if err != nil {
				return "", err
			}
			return cv + "+" + ev, nil
		})

		startWorker(t, testWorker(lgr, q, regV2))
		if err := c.Signal(context.Background(), h.RunID(), "go", "ok"); err != nil {
			t.Fatalf("signal: %v", err)
		}
		out, err := h.Result(resultCtx(t))
		if err != nil {
			t.Fatalf("result: %v", err)
		}
		if want := "C(A(in))+ok"; out != want {
			t.Fatalf("output = %q, want %q", out, want)
		}
		if aN.Load() != 1 {
			t.Errorf("a executed %d times, want 1: records must memoize across versions", aN.Load())
		}
		if cN.Load() != 1 {
			t.Errorf("c executed %d times, want 1", cN.Load())
		}
	})

	t.Run("removed call leaves a harmless record", func(t *testing.T) {
		t.Parallel()
		lgr := ledger.NewMemory()
		q := queue.NewMemory()
		reg := run.NewRegistry()

		var aN atomic.Int64
		aAct := durable.ActivityIn(reg, "a", func(_ context.Context, s string) (string, error) {
			aN.Add(1)
			return "A(" + s + ")", nil
		})
		wf := durable.ActivityIn(reg, "shrunk", func(ctx context.Context, in string) (string, error) {
			return aAct.Call(ctx, in)
		})

		c := durable.New(lgr, q)
		h, err := durable.Start(context.Background(), c, wf, "in")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		// v1 history contains a record for "b", which today's body no
		// longer calls. Written before any worker exists.
		if _, err := lgr.Record(context.Background(), run.Record{
			RunID: h.RunID(), Key: "b#0", Kind: run.KindActivity, Name: "b",
			InputHash: run.HashInput([]byte(`"in"`)),
			Status:    run.RecordOK, Output: []byte(`"leftover"`),
		}); err != nil {
			t.Fatalf("record leftover: %v", err)
		}

		startWorker(t, testWorker(lgr, q, reg))
		out, err := h.Result(resultCtx(t))
		if err != nil {
			t.Fatalf("result: %v (a leftover record is a warning, not an error)", err)
		}
		if out != "A(in)" {
			t.Fatalf("output = %q, want %q", out, "A(in)")
		}
		if aN.Load() != 1 {
			t.Errorf("a executed %d times, want 1", aN.Load())
		}
	})

	t.Run("changed input fails non-deterministic", func(t *testing.T) {
		t.Parallel()
		lgr := ledger.NewMemory()
		q := queue.NewMemory()
		reg := run.NewRegistry()

		var aN atomic.Int64
		aAct := durable.ActivityIn(reg, "a", func(_ context.Context, s string) (string, error) {
			aN.Add(1)
			return s, nil
		})
		wf := durable.ActivityIn(reg, "diverge", func(ctx context.Context, _ string) (string, error) {
			return aAct.Call(ctx, "live-input")
		})

		c := durable.New(lgr, q)
		h, err := durable.Start(context.Background(), c, wf, "x", durable.WithMaxAttempts(5))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		// The recorded call saw a different input than today's code sends.
		oldInput, err := json.Marshal("recorded-input")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := lgr.Record(context.Background(), run.Record{
			RunID: h.RunID(), Key: "a#0", Kind: run.KindActivity, Name: "a",
			InputHash: run.HashInput(oldInput),
			Status:    run.RecordOK, Output: []byte(`"stale"`),
		}); err != nil {
			t.Fatalf("record stale: %v", err)
		}

		startWorker(t, testWorker(lgr, q, reg))
		_, err = h.Result(resultCtx(t))
		var rfe *durable.RunFailedError
		if !errors.As(err, &rfe) {
			t.Fatalf("result error = %v, want *RunFailedError", err)
		}
		if !strings.Contains(rfe.Message, "non-deterministic") {
			t.Fatalf("failure message = %q, want non-deterministic", rfe.Message)
		}
		if aN.Load() != 0 {
			t.Errorf("a executed %d times, want 0: divergence is detected at replay, not execution", aN.Load())
		}
		r, _, err := lgr.Load(context.Background(), h.RunID())
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if r.Attempt != 1 {
			t.Errorf("terminal attempt = %d, want 1: non-determinism must not burn retries", r.Attempt)
		}
	})
}
