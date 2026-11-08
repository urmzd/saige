package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable/adapters/postgres/pgledger"
	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
	drun "github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn/native"
)

// ── fixtures ────────────────────────────────────────────────────────

// stopRecords returns the stop requests recorded on a run in the ledger.
func (w *world) stopRecords(t *testing.T, runID string) []event.StopRequest {
	t.Helper()
	_, records, err := w.ledger.Load(context.Background(), runID)
	if errors.Is(err, drun.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("Load(%s): %v", runID, err)
	}
	var out []event.StopRequest
	for _, rec := range records {
		if !strings.HasPrefix(rec.Key, "event:fiber.stop") {
			continue
		}
		var body event.StopRequest
		if err := json.Unmarshal(rec.Output, &body); err != nil {
			t.Fatalf("decode stop record: %v", err)
		}
		out = append(out, body)
	}
	return out
}

// controlEvents returns every stop request published to the bus.
func (w *world) controlEvents() []event.Envelope {
	var out []event.Envelope
	for _, e := range w.bus.rec.all() {
		if event.IsControl(e.Channel) {
			out = append(out, e)
		}
	}
	return out
}

// noise publishes one event to each of n runs nobody is interested in.
func noise(t *testing.T, b bus.Bus, n int) {
	t.Helper()
	for i := range n {
		run := fmt.Sprintf("run_%d", i)
		if err := b.Publish(context.Background(), event.Envelope{
			ID: run + ":1", Source: "fiber://run/" + run, Type: event.TypeText,
			Channel: "github://someone/else", Tenant: "acme", Run: run, Seq: 1,
			Agent: "someone-else", Data: event.Text{Text: "noise"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// in returns tg answering in its own thread, so two runs of one test do not
// share a conversation.
func in(tg fixture, thread string) fixture {
	tg.Source = address.MustParse("github://urmzd/fiber/issues/" + thread)
	return tg
}

// entriesOf returns a run's own entries in its thread.
func entriesOf(t *testing.T, s thread.Store, tg fixture, runID string) []string {
	t.Helper()
	var mine []thread.Entry
	for _, e := range journal(t, s, tg.key()) {
		if e.RunID == runID {
			mine = append(mine, e)
		}
	}
	return said(mine)
}

// ── the durable record ──────────────────────────────────────────────

// A stop is recorded on the run before it is published, so a bus that has
// forgotten the request by the time a worker gets to the run does not matter:
// the attempt reads the ledger first and ends the turn without calling a
// model.
func TestAStopTheBusEvictedIsStillHonouredFromTheLedger(t *testing.T) {
	broker := bus.NewMemory(bus.WithMaxRuns(4))
	w := newWorldOn(t, broker)
	model := newHeldModel("never said")
	rt := w.runtime(t, sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model}))

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")
	noise(t, broker, 16) // evicts the request, and the run's own stream with it

	w.work(t, rt, nil)
	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "Stopped by a user" {
		t.Fatalf("Output = %+v: the recorded request was not honoured", out)
	}
	if n := len(model.asked()); n != 0 {
		t.Errorf("the model was called %d times after an acknowledged stop", n)
	}
}

// A stop outlives the process that accepted it. The bus that carried it is
// gone with the process; the ledger, queue and journal are not, and the next
// process's first attempt ends stopped without calling the executor.
func TestAStopSurvivesAWorkerRestart(t *testing.T) {
	w := newWorld(t)
	before := newPatient("never runs")
	rt := w.runtime(t, before)

	tg := tag("docs-bot", "evt-1", "explain")
	runID := w.accept(t, rt, tg)
	w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")

	after := w.restarted()
	exec := newPatient("never runs either")
	rt2 := after.runtime(t, exec)
	after.work(t, rt2, nil)

	out := after.result(t, rt2, runID)
	if out.Stopped == nil || out.Stopped.Reason != "Stopped by a user" || out.Stopped.By != "U1" {
		t.Fatalf("Output = %+v, want the request made before the restart", out)
	}
	if before.count()+exec.count() != 0 {
		t.Errorf("an executor ran %d times for a run stopped before it started", before.count()+exec.count())
	}
	ended(t, after.bus.of(runID))
	want := []string{"user: explain", "assistant: [This turn was stopped before it finished.]"}
	if got := said(journal(t, w.threads, tg.key())); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
}

// A running turn whose bus never delivers the request still stops: it reads
// the ledger on a poll. The request here is made through a process on a
// different bus, which is a partition as far as the running turn can tell.
func TestARunningTurnStopsFromTheLedgerWhenTheBusMissesTheRequest(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ")
	executing := w.runtime(t, exec, runtime.WithStopPoll(20*time.Millisecond))
	w.work(t, executing, nil)

	elsewhere := w.restarted()
	edge := elsewhere.runtime(t, newPatient())

	runID := w.accept(t, executing, tag("docs-bot", "evt-1", "explain"))
	exec.inFlight(t)
	elsewhere.stop(t, edge, "acme", runID, "Stopped elsewhere", "U1")

	out := w.result(t, executing, runID)
	if out.Stopped == nil || out.Stopped.Reason != "Stopped elsewhere" {
		t.Fatalf("Output = %+v, want the request the bus never delivered", out)
	}
	ended(t, w.bus.of(runID))
	if n := len(w.controlEvents()); n != 0 {
		t.Fatalf("the running turn's bus carried %d requests; the test needs it to carry none", n)
	}
}

// stopsAsItFinishes is an executor whose turn is stopped in the instant
// between finishing its answer and returning it: the request is recorded,
// and never reaches a turn that has already finished.
type stopsAsItFinishes struct {
	stop func(runID string)
}

func (s *stopsAsItFinishes) Execute(_ context.Context, req runtime.Request, _ func(context.Context, event.Body) error) (runtime.Outcome, error) {
	s.stop(req.RunID)
	return runtime.Outcome{Text: "The whole answer.", Prompt: req.Text}, nil
}

// The other edge of "stopped means cut short": a request recorded in the
// instant the turn finished its answer, which never reached the turn, leaves
// the run completed. Journal, terminal event and result say completed, and
// the recorded request is claimed rather than left behind in the ledger.
func TestAStopThatLandsAsTheTurnFinishesLeavesItCompleted(t *testing.T) {
	w := newWorld(t)
	exec := &stopsAsItFinishes{}
	rt := w.runtime(t, exec)
	exec.stop = func(runID string) { w.stop(t, rt, "acme", runID, "just too late", "U1") }
	w.work(t, rt, nil)

	tg := tag("docs-bot", "evt-1", "explain")
	runID := w.accept(t, rt, tg)
	out := w.result(t, rt, runID)
	if out.Stopped != nil || out.Text != "The whole answer." {
		t.Fatalf("Output = %+v, want the finished answer, completed", out)
	}
	events := w.bus.of(runID)
	last := events[len(events)-1]
	if last.Type != event.TypeCompleted {
		t.Fatalf("last event %s, want completed: %v", last.Type, kindsOf(events))
	}
	want := []string{"user: explain", "assistant: The whole answer."}
	if got := entriesOf(t, w.threads, tg, runID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
	if got := w.stopRecords(t, runID); len(got) != 1 {
		t.Fatalf("stop records = %+v, want the one request", got)
	}
}

// A stop racing an ordinary completion resolves one way, everywhere: the
// journal, the terminal event, the ledger's result and Result all say the same
// thing, whichever way it went. Run many times so both outcomes occur.
func TestAStopRacingCompletionEndsExactlyOneWayEverywhere(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("The answer")
	close(exec.finish)
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	outcomes := map[bool]int{}
	for i := range 25 {
		tg := in(tag("docs-bot", fmt.Sprintf("evt-%d", i), "explain"), fmt.Sprint(i))
		runID := w.accept(t, rt, tg)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rt.Stop(context.Background(), runtime.StopRequest{Tenant: "acme", RunID: runID, Reason: "race"})
		}()
		out := w.result(t, rt, runID)
		wg.Wait()
		stopped := out.Stopped != nil
		outcomes[stopped]++

		events := w.bus.of(runID)
		var terminal []string
		for _, e := range events {
			if e.Type == event.TypeCompleted || e.Type == event.TypeStopped {
				terminal = append(terminal, e.Type)
			}
		}
		last := events[len(events)-1]
		entries := entriesOf(t, w.threads, tg, runID)
		if len(entries) != 2 {
			t.Fatalf("run %d journaled %q, want one exchange", i, entries)
		}
		if stopped {
			answer := runtime.StopMarker
			if out.Text != "" {
				answer = out.Text + "\n\n" + runtime.StopMarker
			}
			if len(terminal) != 1 || last.Type != event.TypeStopped || stoppedBody(t, last).Text != out.Text ||
				entries[1] != "assistant: "+answer {
				t.Fatalf("run %d stopped but disagrees: terminal %v, last %s, journal %q, result %+v", i, terminal, last.Type, entries, out)
			}
		} else {
			body, err := decode[lifecycle](last)
			if err != nil || len(terminal) != 1 || last.Type != event.TypeCompleted || body.Text != out.Text ||
				entries[1] != "assistant: "+out.Text {
				t.Fatalf("run %d completed but disagrees: terminal %v, last %s, journal %q, result %+v", i, terminal, last.Type, entries, out)
			}
		}
		rec, err := w.ledger.GetRun(context.Background(), runID)
		if err != nil || rec.Status != drun.StatusSucceeded {
			t.Fatalf("run %d ledger = %+v, %v", i, rec.Status, err)
		}
	}
	t.Logf("stopped %d, completed %d", outcomes[true], outcomes[false])
}

// The journal keeps the first write of an exchange and ignores later ones, so
// what protects it is that only one outcome is ever offered. A completed run
// that journaled its answer, then failed to announce it, and was asked to stop
// before the retry, stays completed: the retry replays the recorded answer,
// which was a finished one.
func TestACompletedRunRetriedAfterItsJournalEntryStaysCompleted(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("The whole answer")
	close(exec.finish)
	rt := w.runtime(t, exec)

	var once sync.Once
	w.bus.rec.failWhen(func(e event.Envelope) error {
		var err error
		if e.Type == event.TypeCompleted {
			once.Do(func() { err = errors.New("broker outage") })
		}
		return err
	})
	tg := tag("docs-bot", "evt-1", "explain")
	runID := tg.runID()
	var asked sync.Once
	w.work(t, rt, func() {
		asked.Do(func() { w.stop(t, rt, "acme", runID, "after the answer", "U1") })
	})
	w.accept(t, rt, tg)

	out := w.result(t, rt, runID)
	if out.Stopped != nil || out.Text != "The whole answer and the rest." {
		t.Fatalf("Output = %+v, want the completed answer", out)
	}
	events := w.bus.of(runID)
	if last := events[len(events)-1]; last.Type != event.TypeCompleted {
		t.Fatalf("last event %s, want completed: %v", last.Type, kindsOf(events))
	}
	for _, e := range events {
		if e.Type == event.TypeStopped {
			t.Fatalf("a completed run was also announced stopped: %v", kindsOf(events))
		}
	}
	want := []string{"user: explain", "assistant: The whole answer and the rest."}
	if got := said(journal(t, w.threads, tg.key())); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
	if n := exec.count(); n != 1 {
		t.Errorf("the executor ran %d times, want 1", n)
	}
}

// What the ledger holds is bounded by the runs it already holds. A request is
// recorded only for a run that exists, is the asker's, and has not ended, at
// most once per run, and it lives as long as the run's other records. A
// request for anything else writes nothing and publishes nothing, and reads
// exactly like one that did.
func TestAStopIsRecordedOnlyForALiveRunTheAskerOwnsAndAtMostOnce(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("The answer")
	close(exec.finish)
	rt := w.runtime(t, exec)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	unknown := runtime.RunKey("acme", "never-accepted", "docs-bot")

	w.stop(t, rt, "globex", runID, "theirs", "intruder")
	w.stop(t, rt, "acme", unknown, "nothing", "U1")
	if got := w.stopRecords(t, runID); len(got) != 0 {
		t.Fatalf("another tenant's request was recorded: %+v", got)
	}
	if _, err := w.ledger.GetRun(context.Background(), unknown); !errors.Is(err, drun.ErrNotFound) {
		t.Fatalf("a request for an unknown run created something in the ledger: %v", err)
	}
	if got := w.controlEvents(); len(got) != 0 {
		t.Fatalf("requests nobody may honour were published: %d", len(got))
	}

	if _, asked, err := rt.StopRequested(context.Background(), "acme", runID); err != nil || asked {
		t.Fatalf("StopRequested before any request = %v, %v", asked, err)
	}
	w.stop(t, rt, "acme", runID, "first", "U1")
	w.stop(t, rt, "acme", runID, "second", "U2")
	if got, asked, err := rt.StopRequested(context.Background(), "acme", runID); err != nil || !asked || got.Reason != "first" || got.By != "U1" || got.At.IsZero() {
		t.Fatalf("StopRequested = %+v, %v, %v; want the first request", got, asked, err)
	}
	if _, asked, err := rt.StopRequested(context.Background(), "globex", runID); err != nil || asked {
		t.Fatalf("another tenant read the request: %v, %v", asked, err)
	}
	got := w.stopRecords(t, runID)
	if len(got) != 1 || got[0].Reason != "first" || got[0].By != "U1" || got[0].At.IsZero() {
		t.Fatalf("records = %+v, want the first request once, with when it was made", got)
	}

	w.work(t, rt, nil)
	if out := w.result(t, rt, runID); out.Stopped == nil || out.Stopped.Reason != "first" {
		t.Fatalf("Output = %+v", out)
	}
	published := len(w.controlEvents())
	w.stop(t, rt, "acme", runID, "after the end", "U3")
	if got := w.stopRecords(t, runID); len(got) != 1 {
		t.Errorf("a request for a finished run was recorded: %+v", got)
	}
	if n := len(w.controlEvents()); n != published {
		t.Errorf("a request for a finished run was published")
	}
}

// ── the bus, and what it keeps ──────────────────────────────────────

// The reproduction from review: another tenant floods a run's control topic
// before the run starts. With per-tenant streams the flood lands in the
// flooder's own stream for that run id, and with the ledger it is not
// published at all, so the tenant's own request is still there.
func TestAnotherTenantsFloodCannotEvictAPendingStop(t *testing.T) {
	for _, busOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("bus only %v", busOnly), func(t *testing.T) {
			w := newWorldOn(t, newBroker(t, bus.WithRetention(8)))
			w.busOnly = busOnly
			exec := newPatient("Half ")
			exec.patience = 2 * time.Second
			rt := w.runtime(t, exec)

			runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
			w.stop(t, rt, "acme", runID, "ours", "U1")
			for i := range 20 {
				w.stop(t, rt, "globex", runID, fmt.Sprintf("flood %d", i), "intruder")
			}
			w.work(t, rt, nil)
			out := w.result(t, rt, runID)
			if out.Stopped == nil || out.Stopped.Reason != "ours" {
				t.Fatalf("Output = %+v: the flood evicted the tenant's own request", out)
			}
		})
	}
}

// Without the ledger the bus is all there is, and these are its exact bounds
// on the in-memory bus. The depth of a run's stream cannot lose a request,
// because only the run's own tenant writes there and the newest requests are
// the ones kept; it only decides which reason survives. What can lose one is
// the in-memory bus forgetting the whole stream: it keeps the WithMaxRuns
// streams written most recently, so a request is lost once WithMaxRuns other
// streams have been written after it. The run's own stream is one of them:
// the attempt announces itself before it listens.
func TestWithoutTheLedgerTheMemoryBusBoundsAPendingStop(t *testing.T) {
	const maxRuns = 6
	t.Run("the depth keeps the newest requests", func(t *testing.T) {
		w := newWorldOn(t, bus.NewMemory(bus.WithRetention(2)))
		w.busOnly = true
		exec := newPatient()
		rt := w.runtime(t, exec)
		runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
		for _, reason := range []string{"one", "two", "three"} {
			w.stop(t, rt, "acme", runID, reason, "U1")
		}
		w.work(t, rt, nil)
		if out := w.result(t, rt, runID); out.Stopped == nil || out.Stopped.Reason != "two" {
			t.Fatalf("Output = %+v, want the oldest request still retained", out)
		}
	})
	for _, tc := range []struct {
		others  int
		stopped bool
	}{{maxRuns - 2, true}, {maxRuns - 1, false}} {
		// noise writes tc.others streams; the attempt's own announcement
		// is one more.
		t.Run(fmt.Sprintf("%d other streams written after it", tc.others+1), func(t *testing.T) {
			broker := bus.NewMemory(bus.WithMaxRuns(maxRuns))
			w := newWorldOn(t, broker)
			w.busOnly = true
			exec := newPatient("Half")
			exec.patience = 500 * time.Millisecond
			rt := w.runtime(t, exec)
			runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
			w.stop(t, rt, "acme", runID, "ours", "U1")
			noise(t, broker, tc.others)
			w.work(t, rt, nil)
			out := w.result(t, rt, runID)
			if got := out.Stopped != nil; got != tc.stopped {
				t.Fatalf("stopped = %v, want %v: Output %+v", got, tc.stopped, out)
			}
		})
	}
}

// ── robustness ──────────────────────────────────────────────────────

// panicky panics on its first execution and answers on the next.
type panicky struct {
	mu   sync.Mutex
	runs int
}

func (p *panicky) Execute(ctx context.Context, req runtime.Request, emit func(context.Context, event.Body) error) (runtime.Outcome, error) {
	p.mu.Lock()
	p.runs++
	first := p.runs == 1
	p.mu.Unlock()
	if first {
		panic("executor bug")
	}
	return runtime.Outcome{Text: "fine", Prompt: req.Text}, nil
}

// An executor that panics is recovered by the worker and retried. Everything
// the stop path started for the attempt (the relay, the bus listener, the
// ledger poll) ends with it, so the process is left with the goroutines it had.
func TestAPanickingExecutorLeavesNoStopGoroutinesBehind(t *testing.T) {
	w := newWorldOn(t, bus.NewMemory())
	exec := &panicky{}
	rt := w.runtime(t, exec, runtime.WithStopPoll(5*time.Millisecond))
	w.work(t, rt, nil)

	// Settle the worker's own goroutines before counting.
	warm := w.accept(t, rt, tag("docs-bot", "evt-warm", "warm up"))
	_ = w.result(t, rt, warm)
	baseline := goruntime.NumGoroutine()

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	if out := w.result(t, rt, runID); out.Text != "fine" || out.Stopped != nil {
		t.Fatalf("Output = %+v, want the retried attempt's answer", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for goruntime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := goruntime.Stack(buf, true)
			t.Fatalf("%d goroutines, baseline %d:\n%s", goruntime.NumGoroutine(), baseline, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// However a turn is stopped (before it began, by the grace-period cancel, or
// wrapped up by the agent loop) its request is journaled as the same words:
// the request as the agent renders it.
func TestEveryWayOfStoppingRecordsTheSameRequest(t *testing.T) {
	meta := map[string]string{"channel": "ops"}
	want := "user: Context from the trigger:\n- channel: ops\n\nexplain"
	mk := func(id string) fixture {
		tg := in(tag("docs-bot", id, "explain"), id)
		tg.Meta = meta
		return tg
	}

	t.Run("before it began", func(t *testing.T) {
		w := newWorld(t)
		rt := w.runtime(t, newPatient())
		tg := mk("evt-before")
		runID := w.accept(t, rt, tg)
		w.stop(t, rt, "acme", runID, "", "")
		w.work(t, rt, nil)
		out := w.result(t, rt, runID)
		if out.Stopped == nil || out.Stopped.Reason != native.DefaultStopReason {
			t.Fatalf("Output.Stopped = %+v, want the default reason", out.Stopped)
		}
		if got := entriesOf(t, w.threads, tg, runID); len(got) != 2 || got[0] != want {
			t.Fatalf("journal = %q", got)
		}
	})
	t.Run("cancelled after the grace period", func(t *testing.T) {
		w := newWorld(t)
		exec := newPatient("Half")
		exec.deaf = true
		rt := w.runtime(t, exec, runtime.WithStopGrace(10*time.Millisecond))
		w.work(t, rt, nil)
		tg := mk("evt-grace")
		runID := w.accept(t, rt, tg)
		exec.inFlight(t)
		w.stop(t, rt, "acme", runID, "", "")
		out := w.result(t, rt, runID)
		if out.Stopped == nil || out.Stopped.Reason != native.DefaultStopReason {
			t.Fatalf("Output.Stopped = %+v, want the default reason", out.Stopped)
		}
		if got := entriesOf(t, w.threads, tg, runID); len(got) != 2 || got[0] != want {
			t.Fatalf("journal = %q", got)
		}
	})
	t.Run("wrapped up by the agent loop", func(t *testing.T) {
		w := newWorld(t)
		model := newHeldModel("Half")
		rt := w.runtime(t, sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model}))
		w.work(t, rt, nil)
		tg := mk("evt-loop")
		runID := w.accept(t, rt, tg)
		select {
		case <-model.started:
		case <-time.After(20 * time.Second):
			t.Fatal("the model was never called")
		}
		w.stop(t, rt, "acme", runID, "", "")
		out := w.result(t, rt, runID)
		if out.Stopped == nil || out.Stopped.Reason != native.DefaultStopReason {
			t.Fatalf("Output.Stopped = %+v, want the default reason", out.Stopped)
		}
		if got := entriesOf(t, w.threads, tg, runID); len(got) != 2 || got[0] != want {
			t.Fatalf("journal = %q", got)
		}
	})
}

// sandboxWithPlane is sandboxOf, keeping the plane so a test can submit to it
// directly.
func sandboxWithPlane(t *testing.T, sp *specs, model saigetypes.Provider) (*runtime.Sandbox, controlplane.ControlPlane) {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	s, err := runtime.NewSandbox(plane, tools, sp, runtime.WithAgentOptions(
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil })))
	if err != nil {
		t.Fatal(err)
	}
	return s, plane
}

// A task whose turn is no longer awaited is refused before it calls a model:
// whoever submitted it has gone, and its answer would be read by nobody.
func TestTheSandboxRefusesATaskWhoseTurnIsGone(t *testing.T) {
	sp := newSpecs()
	rev := sp.revise("docs-bot", "You document things.")
	model := newHeldModel("", agentTextOnce("deployed")...)
	s, plane := sandboxWithPlane(t, sp, model)

	// One ordinary turn deploys the revision. The held model's first call
	// is held, so stop it with an already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = s.Execute(ctx, withPin(runtime.Request{RunID: "warm", Tenant: "acme", Text: "x"}, rev), nil)

	before := len(model.asked())
	input, err := json.Marshal(withPin(runtime.Request{RunID: "nobody-waits", Tenant: "acme", Text: "x"}, rev))
	if err != nil {
		t.Fatal(err)
	}
	deployment := runtime.DeploymentName(withPin(runtime.Request{}, rev).Pin)
	id, err := plane.SubmitAsync(context.Background(), deployment, task.Task{Tool: runtime.ToolName("docs-bot"), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, done, err := plane.Result(context.Background(), deployment, id)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			if !strings.Contains(res.Error, "no longer awaited") {
				t.Fatalf("result = %+v, want the task refused", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(model.asked()); n != before {
		t.Errorf("a task nobody waited on called the model %d times", n-before)
	}
}

func agentTextOnce(s string) [][]saigetypes.Delta {
	return [][]saigetypes.Delta{saigetypes.PartDeltas(0, saigetypes.Text(s))}
}

// Cancelling the turn that waits on a task cancels the task, for any reason,
// not only a stop: a worker shutting down must not leave a model call running
// behind it.
func TestCancellingAWaitingTurnCancelsItsTask(t *testing.T) {
	sp := newSpecs()
	rev := sp.revise("docs-bot", "You document things.")
	model := newHeldModel("Half")
	s, _ := sandboxWithPlane(t, sp, model)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Execute(ctx, withPin(runtime.Request{RunID: "run-1", Tenant: "acme", Text: "x"}, rev),
			func(context.Context, event.Body) error { return nil })
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was never called")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v, want the cancellation", err)
	}
	select {
	case <-model.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the model call kept running after the turn waiting on it was cancelled")
	}
}

// The integration job sets $SAIGE_TEST_POSTGRES_DSN and requires this test to
// pass, which is how it knows every stop test above, and the usage tests in
// usage_test.go, ran on Postgres (ledger, queue, journal) rather than on the
// in-memory fallbacks. Without it the test skips, so plain `go test ./...`
// stays hermetic.
func TestStopTestsRunOnPostgres(t *testing.T) {
	if os.Getenv(threadsEnv) == "" {
		t.Skipf("set %s to run the stop tests on Postgres", threadsEnv)
	}
	w := newWorld(t)
	if _, ok := w.ledger.(*pgledger.Ledger); !ok {
		t.Fatalf("the ledger is %T, want Postgres", w.ledger)
	}
	if _, ok := w.queue.(*pgqueue.Queue); !ok {
		t.Fatalf("the queue is %T, want Postgres", w.queue)
	}
	if _, ok := w.threads.Store.(*thread.Postgres); !ok {
		t.Fatalf("the journal is %T, want Postgres", w.threads.Store)
	}
	t.Logf("stop tests run on ledger %T, queue %T, bus %T, journal %T", w.ledger, w.queue, w.bus.Bus, w.threads.Store)
}

// causeRecorder is a Delegator that waits until its context ends and keeps the
// cause it ended with.
type causeRecorder struct {
	started chan struct{}
	cause   chan error
}

func newCauseRecorder() *causeRecorder {
	return &causeRecorder{started: make(chan struct{}), cause: make(chan error, 1)}
}

func (c *causeRecorder) delegate(ctx context.Context, _ runtime.Request, _, _ string, _ func(event.Action) error) (string, error) {
	close(c.started)
	<-ctx.Done()
	c.cause <- context.Cause(ctx)
	return "", ctx.Err()
}

func leadWithSpawn(sp *specs) native.Revision {
	sp.revise("helper", "Help.")
	return sp.set(native.Spec{
		Name: "lead", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "Lead.",
		Access: native.Access{Spawn: []string{"helper"}},
	})
}

func delegatingLead() *agenttest.ScriptedProvider {
	return &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("call-1", runtime.DelegateToolName("helper"), map[string]any{"text": "Count the failing tests"}),
		agenttest.TextResponse("never reached"),
	}}
}

// A host that runs delegations as runs of their own must know why its
// context ended. A stop ends it with a *StoppedError carrying the request, so
// the host can stop the child with the same words; a worker that abandons the
// turn for any other reason ends it with that reason instead.
func TestADelegatorCanTellAStopFromAnyOtherCancellation(t *testing.T) {
	t.Run("stopped", func(t *testing.T) {
		w := newWorld(t)
		leadWithSpawn(w.specs)
		rec := newCauseRecorder()
		exec := sandboxOf(t, w.specs, map[string]saigetypes.Provider{"lead": delegatingLead()}, runtime.WithDelegator(rec.delegate))
		rt := w.runtime(t, exec)
		w.work(t, rt, nil)

		runID := w.accept(t, rt, tag("lead", "evt-1", "how are the tests?"))
		<-rec.started
		w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")
		cause := <-rec.cause
		var stopped *runtime.StoppedError
		if !errors.Is(cause, runtime.ErrStopped) || !errors.As(cause, &stopped) || stopped.Reason != "Stopped by a user" || stopped.By != "U1" {
			t.Fatalf("cause = %v, want the stop request", cause)
		}
		if out := w.result(t, rt, runID); out.Stopped == nil {
			t.Fatalf("Output = %+v, want stopped", out)
		}
	})
	t.Run("abandoned", func(t *testing.T) {
		sp := newSpecs()
		lead := leadWithSpawn(sp)
		rec := newCauseRecorder()
		exec := sandboxOf(t, sp, map[string]saigetypes.Provider{"lead": delegatingLead()}, runtime.WithDelegator(rec.delegate))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = exec.Execute(ctx, withPin(runtime.Request{RunID: "run-1", Tenant: "acme", Text: "x"}, lead),
				func(context.Context, event.Body) error { return nil })
		}()
		<-rec.started
		cancel()
		cause := <-rec.cause
		<-done
		if errors.Is(cause, runtime.ErrStopped) || !errors.Is(cause, context.Canceled) {
			t.Fatalf("cause = %v, want a plain cancellation", cause)
		}
	})
}

// warnings is a logger that keeps every record at warning level or above.
type warnings struct {
	mu   sync.Mutex
	seen []string
}

func (w *warnings) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, r.Message)
	return nil
}
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }
func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

// An ordinary stop is not an incident. Neither the runtime nor durable's
// worker warns about anything, whether the request was recorded during the
// turn (it is not in the attempt's replay set) or before it (the workflow body
// claims it).
func TestAStopLogsNoWarning(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded before the attempt %v", before), func(t *testing.T) {
			w := newWorld(t)
			seen := &warnings{}
			w.log = slog.New(seen)
			exec := newPatient("Half an ")
			rt := w.runtime(t, exec)
			runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
			if before {
				w.stop(t, rt, "acme", runID, "early", "U1")
			}
			w.work(t, rt, nil)
			if !before {
				exec.inFlight(t)
				w.stop(t, rt, "acme", runID, "live", "U1")
			}
			if out := w.result(t, rt, runID); out.Stopped == nil {
				t.Fatalf("Output = %+v, want stopped", out)
			}
			time.Sleep(50 * time.Millisecond) // the worker logs after it settles the run
			if got := seen.all(); len(got) != 0 {
				t.Fatalf("a stop logged warnings: %q", got)
			}
		})
	}
}
