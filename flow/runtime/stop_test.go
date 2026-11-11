package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/ledger"
	dqueue "github.com/urmzd/saige/durable/queue"
	drun "github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/durable/adapters/postgres/pgkit"
	"github.com/urmzd/saige/durable/adapters/postgres/pgledger"
	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn/native"
)

// ── fixtures ────────────────────────────────────────────────────────

// newBroker returns an empty in-memory bus, a complete implementation of the
// bus contract.
func newBroker(t *testing.T, opts ...bus.Option) bus.Bus {
	t.Helper()
	return bus.NewMemory(opts...)
}

// tap is a real broker with a record of everything published to it. The stop
// request travels over a real broker in these tests, because the broker's
// retention is the thing that settles when a request may arrive.
type tap struct {
	bus.Bus
	rec recorder
}

func (b *tap) Publish(ctx context.Context, e event.Envelope) error {
	if err := b.rec.Publish(ctx, e); err != nil {
		return err
	}
	return b.Bus.Publish(ctx, e)
}

// of returns what one run published, in order. Requests addressed to the run
// are on another stream and are not among them.
func (b *tap) of(runID string) []event.Envelope {
	var out []event.Envelope
	for _, e := range b.rec.all() {
		if e.Run == runID && !event.IsControl(e.Channel) {
			out = append(out, e)
		}
	}
	return out
}

// spied is a thread store that keeps every append it was asked to make, so a
// test can compare what a replay offered the journal with what the first
// attempt did.
type spied struct {
	thread.Store
	mu      sync.Mutex
	appends [][]thread.Entry
}

func (s *spied) Append(ctx context.Context, key thread.Key, entries ...thread.Entry) ([]thread.Entry, error) {
	s.mu.Lock()
	s.appends = append(s.appends, append([]thread.Entry(nil), entries...))
	s.mu.Unlock()
	return s.Store.Append(ctx, key, entries...)
}

func (s *spied) offered() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.appends))
	for i, call := range s.appends {
		out[i] = said(call)
	}
	return out
}

// durableLedger is the run ledger as a deployment builds it: the ledger plus
// the point read a result poll uses.
type durableLedger interface {
	ledger.Ledger
	ledger.RunGetter
}

var durableSchemas atomic.Uint64

// newDurable returns an empty run ledger and queue: the durable engine's
// suggested Postgres tables in a schema of its own when $SAIGE_TEST_POSTGRES_DSN is set,
// durable's in-memory ones otherwise. It is the ledger a stop is recorded in.
func newDurable(t *testing.T) (durableLedger, dqueue.Queue, runtime.RecordReader) {
	t.Helper()
	url := os.Getenv(threadsEnv)
	if url == "" {
		return ledger.NewMemory(), dqueue.NewMemory(), nil
	}
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_stop_%d_%d", time.Now().UnixNano(), durableSchemas.Add(1))
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse $%s: %v", threadsEnv, err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		pool.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
		pool.Close()
	})
	lm, qm := pgledger.DefaultMapping(), pgqueue.DefaultMapping()
	if err := pgkit.Apply(ctx, pool, "flow.runtime.test", pgledger.RecommendedDDL(lm), pgqueue.RecommendedDDL(qm)); err != nil {
		t.Fatalf("apply the durable tables: %v", err)
	}
	lgr, err := pgledger.New(pool, lm)
	if err != nil {
		t.Fatalf("pgledger.New: %v", err)
	}
	q, err := pgqueue.New(pool, qm)
	if err != nil {
		t.Fatalf("pgqueue.New: %v", err)
	}
	if err := errors.Join(lgr.Validate(ctx), q.Validate(ctx)); err != nil {
		t.Fatalf("validate the durable tables: %v", err)
	}
	// pgledger reads records by prefix itself (ledger.PrefixReader).
	return lgr, q, lgr
}

// world is the storage several runtimes share: one ledger, one queue, one bus,
// one journal. Two runtimes over one world are two workers of one deployment.
//
// Runtimes record stop requests in the ledger, as a deployment's do, unless
// busOnly is set, which is a host that never passed WithStopLedger.
type world struct {
	ledger durableLedger
	queue  dqueue.Queue
	// reader reads records by prefix: Postgres's, nil in memory.
	reader  runtime.RecordReader
	bus     *tap
	specs   *specs
	threads *spied
	busOnly bool
	// log, when set, receives what the runtimes and workers log.
	log *slog.Logger
	// concurrency is how many runs a worker executes at once; zero is two.
	// A run that waits on a run of its own needs more.
	concurrency int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldOn(t, newBroker(t))
}

// newWorldOn is newWorld over a given bus, for a test about that bus's bounds.
func newWorldOn(t *testing.T, b bus.Bus) *world {
	t.Helper()
	lgr, q, reader := newDurable(t)
	w := &world{
		ledger:  lgr,
		queue:   q,
		reader:  reader,
		bus:     &tap{Bus: b},
		specs:   newSpecs(),
		threads: &spied{Store: newThreads(t)},
	}
	w.specs.revise("docs-bot", "You document things.")
	return w
}

// restarted is the same durable state behind a new process: the ledger, the
// queue and the journal survive, and the in-memory bus does not.
func (w *world) restarted() *world {
	again := *w
	again.bus = &tap{Bus: bus.NewMemory()}
	return &again
}

func (w *world) runtime(t *testing.T, exec runtime.Fake, opts ...runtime.Option) *runtime.Runtime {
	t.Helper()
	base := []runtime.Option{runtime.WithThreads(w.threads)}
	if w.log != nil {
		base = append(base, runtime.WithLogger(w.log))
	}
	if !w.busOnly {
		base = append(base, runtime.WithStopLedger(w.ledger))
		if w.reader != nil {
			base = append(base, runtime.WithRecordReader(w.reader))
		}
	}
	opts = append(base, opts...)
	rt, err := runtime.New(durable.New(w.ledger, w.queue), w.specs, w.bus, append([]runtime.Option{runtime.WithRuntime(runtime.AsRuntime(exec))}, opts...)...)
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	return rt
}

// work runs a worker for rt until the test ends. between, when set, is called
// between two attempts of a run, which is the only moment a test can act
// while no attempt exists.
func (w *world) work(t *testing.T, rt *runtime.Runtime, between func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		logger := w.log
		if logger == nil {
			logger = slog.Default()
		}
		_ = worker.New(w.ledger, w.queue,
			worker.WithLogger(logger),
			worker.WithRegistry(rt.Registry()),
			worker.WithConcurrency(max(2, w.concurrency)),
			worker.WithJanitorEvery(0),
			worker.WithLeaseTTL(5*time.Second),
			worker.WithBackoff(func(int) time.Duration {
				if between != nil {
					between()
				}
				return time.Millisecond
			}),
		).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (w *world) accept(t *testing.T, rt *runtime.Runtime, tg fixture) string {
	t.Helper()
	acc, err := accept(context.Background(), rt, tg)
	if err != nil {
		t.Fatalf("Accept(%s): %v", tg.ID, err)
	}
	return acc.RunID
}

func (w *world) result(t *testing.T, rt *runtime.Runtime, runID string) runtime.Output {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := rt.Result(ctx, runID)
	if err != nil {
		t.Fatalf("Result(%s): %v\nevents: %v", runID, err, kindsOf(w.bus.of(runID)))
	}
	return out
}

func (w *world) stop(t *testing.T, rt *runtime.Runtime, tenant, runID, reason, by string) {
	t.Helper()
	if err := rt.Stop(context.Background(), runtime.StopRequest{Tenant: tenant, RunID: runID, Reason: reason, By: by}); err != nil {
		t.Fatalf("Stop(%s, %s): %v", tenant, runID, err)
	}
}

// patient is an executor that says something and then waits: for a stop, for
// the test to let it finish, or to be cancelled. It is the turn a person stops.
type patient struct {
	// says is published as text before the turn waits.
	says []string
	// deaf makes the turn ignore the stop signal, as an executor written
	// before stopping existed would.
	deaf bool
	// patience, when set, is how long a turn waits for a stop before it
	// finishes on its own: long enough for a request the bus holds to
	// arrive, short enough that a lost one shows as a completed run.
	patience time.Duration

	mu       sync.Mutex
	runs     int
	failures int           // executions that fail outright before any succeed
	waiting  chan struct{} // closed once a turn is in flight
	finish   chan struct{} // closed to let a waiting turn complete
	once     sync.Once
}

func newPatient(says ...string) *patient {
	return &patient{says: says, waiting: make(chan struct{}), finish: make(chan struct{})}
}

func (p *patient) Execute(ctx context.Context, req runtime.Request, emit func(context.Context, event.Body) error) (runtime.Outcome, error) {
	p.mu.Lock()
	p.runs++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()
	if fail {
		return runtime.Outcome{}, errors.New("provider unavailable")
	}

	for _, s := range p.says {
		if err := emit(ctx, event.Text{Text: s}); err != nil {
			return runtime.Outcome{}, err
		}
	}
	p.once.Do(func() { close(p.waiting) })

	var signal <-chan native.Stop
	if !p.deaf {
		signal = runtime.StopSignal(ctx)
	}
	partial := strings.Join(p.says, "")
	var timeout <-chan time.Time
	if p.patience > 0 {
		timeout = time.After(p.patience)
	}
	select {
	case <-timeout:
		return runtime.Outcome{Text: partial + " and the rest.", Prompt: req.Text, Chunks: len(p.says)}, nil
	case stop := <-signal:
		return runtime.Outcome{Text: partial, Prompt: req.Text, Chunks: len(p.says), Stopped: &stop}, nil
	case <-p.finish:
		return runtime.Outcome{Text: partial + " and the rest.", Prompt: req.Text, Chunks: len(p.says)}, nil
	case <-ctx.Done():
		return runtime.Outcome{}, ctx.Err()
	}
}

func (p *patient) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

func (p *patient) inFlight(t *testing.T) {
	t.Helper()
	select {
	case <-p.waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never started")
	}
}

// heldModel is a model that says part of an answer and keeps the call open
// until it is cancelled. Later calls answer from a script. It records what it
// was asked, and whether the held call was cancelled.
type heldModel struct {
	partial string
	then    [][]saigetypes.Delta

	mu        sync.Mutex
	requests  []saigetypes.Request
	started   chan struct{}
	cancelled chan struct{}
}

func newHeldModel(partial string, then ...[]saigetypes.Delta) *heldModel {
	return &heldModel{partial: partial, then: then, started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (h *heldModel) Stream(ctx context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	h.mu.Lock()
	n := len(h.requests)
	h.requests = append(h.requests, req)
	h.mu.Unlock()

	ch := make(chan saigetypes.Delta, 8)
	go func() {
		defer close(ch)
		if n > 0 {
			if n-1 < len(h.then) {
				for _, d := range h.then[n-1] {
					ch <- d
				}
			}
			return
		}
		if h.partial != "" {
			for _, d := range saigetypes.PartDeltas(0, saigetypes.Text(h.partial))[:2] {
				ch <- d
			}
		}
		close(h.started)
		<-ctx.Done()
		close(h.cancelled)
	}()
	return ch, nil
}

func (h *heldModel) asked() []saigetypes.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]saigetypes.Request(nil), h.requests...)
}

// sandboxOf builds the real executor over models chosen by agent name.
func sandboxOf(t *testing.T, sp *specs, models map[string]saigetypes.Provider, opts ...runtime.SandboxOption) *runtime.Sandbox {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	opts = append([]runtime.SandboxOption{runtime.WithAgentOptions(
		native.WithProvider(native.ProviderOffline, func(spec native.Spec) (saigetypes.Provider, error) {
			p, ok := models[spec.Name]
			if !ok {
				return nil, fmt.Errorf("no model for %q", spec.Name)
			}
			return p, nil
		}))}, opts...)
	s, err := runtime.NewSandbox(plane, tools, sp, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func stoppedBody(t *testing.T, e event.Envelope) event.Stopped {
	t.Helper()
	body, err := decode[event.Stopped](e)
	if err != nil {
		t.Fatalf("decode %s: %v", e.Type, err)
	}
	return body
}

// ended asserts that a run's stream ends with exactly one stopped event, that
// it carries no completed event and no terminal failure anywhere, and returns
// the stopped event. A failure that was retried may precede the stop: a stop
// between two attempts follows exactly that.
func ended(t *testing.T, events []event.Envelope) event.Envelope {
	t.Helper()
	kinds := kindsOf(events)
	if len(events) == 0 || events[len(events)-1].Type != event.TypeStopped {
		t.Fatalf("the run's last event is not lifecycle.stopped: %v", kinds)
	}
	stopped := 0
	for _, e := range events {
		switch e.Type {
		case event.TypeStopped:
			stopped++
		case event.TypeCompleted:
			t.Fatalf("a stopped run published lifecycle.completed: %v", kinds)
		case event.TypeFailed:
			body, err := decode[lifecycle](e)
			if err != nil || body.Terminal {
				t.Fatalf("a stopped run published a terminal lifecycle.failed: %v", kinds)
			}
		}
	}
	if stopped != 1 {
		t.Fatalf("the run was announced stopped %d times, want 1: %v", stopped, kinds)
	}
	return events[len(events)-1]
}

// ── tests ───────────────────────────────────────────────────────────

// The marker is replayed to every later run in a thread, so its bytes are a
// contract with every journal that already holds it.
func TestTheStopMarkerIsAFixedLine(t *testing.T) {
	if runtime.StopMarker != "[This turn was stopped before it finished.]" {
		t.Fatalf("StopMarker changed to %q: every journal entry written with the old one no longer matches", runtime.StopMarker)
	}
}

// The whole feature, through the real executor and a real agent loop: a person
// stops a run while the model is mid-answer. The run ends stopped, with the
// reason and who asked; what the agent had said is delivered and journaled
// once, followed by the marker; and the reply that comes next is answered by a
// model that was shown the stopped turn.
func TestAStoppedRunKeepsItsPartialAnswerAndTheNextTurnIsShownIt(t *testing.T) {
	w := newWorld(t)
	model := newHeldModel("The deploy failed because", agenttest.TextResponse("It failed on the migration."))
	rt := w.runtime(t, sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model}))
	w.work(t, rt, nil)

	first := tag("docs-bot", "evt-1", "why did the deploy fail?")
	runID := w.accept(t, rt, first)

	// Stop only once the partial answer is on the bus: the run is then
	// demonstrably mid-stream, not about to start.
	waitFor(t, func() bool {
		for _, e := range w.bus.of(runID) {
			if e.Type == event.TypeText {
				return true
			}
		}
		return false
	})
	w.stop(t, rt, "acme", runID, "Stopped by a user", "slack:U1")

	out := w.result(t, rt, runID)
	if out.Stopped == nil || *out.Stopped != (native.Stop{Reason: "Stopped by a user", By: "slack:U1"}) {
		t.Fatalf("Output.Stopped = %+v, want the reason and the asker", out.Stopped)
	}
	if out.Text != "The deploy failed because" {
		t.Errorf("Output.Text = %q, want the partial answer", out.Text)
	}

	events := w.bus.of(runID)
	last := ended(t, events)
	if out.LastSeq != last.Seq {
		t.Errorf("Output.LastSeq = %d, the stopped event is at %d", out.LastSeq, last.Seq)
	}
	body := stoppedBody(t, last)
	// The call that was cut short is counted (see the usage tests); the
	// rest of the body is compared here.
	if body.Usage == nil || body.Usage.Own.ModelCalls != 1 {
		t.Errorf("lifecycle.stopped usage = %+v, want the one call that was cut short", body.Usage)
	}
	body.Usage = nil
	want := event.Stopped{Attempt: 1, Reason: "Stopped by a user", By: "slack:U1", Text: "The deploy failed because"}
	if body != want {
		t.Errorf("lifecycle.stopped = %+v, want %+v", body, want)
	}
	for _, e := range events {
		if e.Type == event.TypeFailed {
			t.Fatalf("a stopped run published lifecycle.failed: %v", kindsOf(events))
		}
	}

	// The journal: the request, then the partial answer and the marker, byte
	// for byte, once.
	key := first.key()
	wantJournal := []string{
		"user: why did the deploy fail?",
		"assistant: The deploy failed because\n\n[This turn was stopped before it finished.]",
	}
	if got := said(journal(t, w.threads, key)); !reflect.DeepEqual(got, wantJournal) {
		t.Fatalf("journal after the stop:\n got %q\nwant %q", got, wantJournal)
	}

	// The reply. It is a new run in the same thread, and its model is shown
	// the stopped exchange ahead of the new request.
	second := tag("docs-bot", "evt-2", "go on, what was it?")
	next := w.accept(t, rt, second)
	if got := w.result(t, rt, next); got.Stopped != nil || got.Text != "It failed on the migration." {
		t.Fatalf("the follow-up run = %+v, want an ordinary completed answer", got)
	}
	asked := model.asked()
	if len(asked) != 2 {
		t.Fatalf("the model was called %d times, want once per run", len(asked))
	}
	msgs := asked[1].Messages
	if len(msgs) != 4 {
		t.Fatalf("the follow-up request has %d messages, want the system prompt, the stopped exchange and the reply", len(msgs))
	}
	if _, ok := msgs[2].(saigetypes.AssistantMessage); !ok {
		t.Fatalf("the stopped answer reached the model as %T, want an assistant message", msgs[2])
	}
	if rendered := fmt.Sprintf("%+v", msgs[2]); !strings.Contains(rendered, "The deploy failed because\n\n[This turn was stopped before it finished.]") {
		t.Errorf("the follow-up was not shown the stopped turn: %s", rendered)
	}
	if rendered := fmt.Sprintf("%+v", msgs); strings.Contains(rendered, "Stopped by a user") || strings.Contains(rendered, "slack:U1") {
		t.Errorf("the reason or the asker of a stop reached a model: %s", rendered)
	}
}

// A stopped run is over. The ledger records it as ended rather than failed, on
// the attempt that was stopped, and the executor is never asked again: nothing
// retries work a person asked to end.
func TestAStoppedRunIsRecordedAsEndedAndIsNeverRetried(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ")
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	exec.inFlight(t)
	w.stop(t, rt, "acme", runID, "", "")
	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Attempt != 1 {
		t.Fatalf("Output = %+v, want a stop on attempt 1", out)
	}

	rec, err := w.ledger.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != drun.StatusSucceeded {
		t.Fatalf("the ledger records the run as %q, want %q: a stopped run did not fail", rec.Status, drun.StatusSucceeded)
	}
	if rec.Error != "" {
		t.Errorf("the ledger records an error for a stopped run: %q", rec.Error)
	}

	// Nothing more happens: no later attempt, no later event.
	before := len(w.bus.of(runID))
	time.Sleep(50 * time.Millisecond)
	ended(t, w.bus.of(runID))
	if after := len(w.bus.of(runID)); after != before {
		t.Errorf("the run published %d more events after it was stopped", after-before)
	}
	if n := exec.count(); n != 1 {
		t.Errorf("the executor ran %d times, want 1", n)
	}
	wantKinds := []string{event.TypeAccepted, event.TypeStarted, event.TypeText, event.TypeStopped}
	if got := kindsOf(w.bus.of(runID)); !sameKinds(got, wantKinds) {
		t.Errorf("events = %v, want %v", got, wantKinds)
	}
}

// Tenant isolation. A request made under another tenant is published and
// never obeyed, and the caller is told exactly what it is told for a run that
// does not exist: nothing. The run then stops for its own tenant's request,
// which is published later and so would lose to the foreign one if that one
// had been honoured at all.
func TestAStopFromAnotherTenantIsNeverHonouredAndReadsLikeAnUnknownRun(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ")
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	exec.inFlight(t)

	foreign := rt.Stop(context.Background(), runtime.StopRequest{Tenant: "globex", RunID: runID, Reason: "theirs", By: "intruder"})
	unknown := rt.Stop(context.Background(), runtime.StopRequest{Tenant: "globex", RunID: "r00000000000000000000000000000000", Reason: "theirs"})
	if foreign != nil || unknown != nil {
		t.Fatalf("Stop for another tenant's run = %v, for an unknown run = %v; both must be nil so neither says whether the run exists", foreign, unknown)
	}

	// The tag's own id, which is what a caller who never saw the run id
	// would guess, addresses nothing at all.
	w.stop(t, rt, "acme", "evt-1", "wrong id", "U9")

	w.stop(t, rt, "acme", runID, "ours", "U1")
	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "ours" || out.Stopped.By != "U1" {
		t.Fatalf("the run stopped for %+v, want its own tenant's request", out.Stopped)
	}
}

// A stop may be asked for before any worker has picked the run up. The bus
// keeps the request, the attempt finds it when it starts, and the run ends
// stopped. The thread still records the exchange: the request, and an answer
// that is only the marker, so the conversation has no request left dangling.
func TestAStopBeforeTheTurnStartsIsNotLost(t *testing.T) {
	w := newWorld(t)
	exec := newPatient() // says nothing before it waits
	rt := w.runtime(t, exec)

	tg := tag("docs-bot", "evt-1", "explain the outage")
	runID := w.accept(t, rt, tg)
	w.stop(t, rt, "acme", runID, "asked too soon", "U1")

	// Only now does a worker exist.
	w.work(t, rt, nil)
	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "asked too soon" {
		t.Fatalf("Output.Stopped = %+v: the early request was lost", out.Stopped)
	}
	if out.Text != "" {
		t.Errorf("Output.Text = %q, want nothing", out.Text)
	}
	ended(t, w.bus.of(runID))

	want := []string{"user: explain the outage", "assistant: [This turn was stopped before it finished.]"}
	if got := said(journal(t, w.threads, tg.key())); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
}

// The same, through the real executor: a request that is waiting when the turn
// starts stops it, and the model is asked at most once and never answers.
func TestAStopBeforeTheTurnStartsStopsARealTurn(t *testing.T) {
	w := newWorld(t)
	model := newHeldModel("")
	rt := w.runtime(t, sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model}))

	tg := tag("docs-bot", "evt-1", "explain the outage")
	runID := w.accept(t, rt, tg)
	w.stop(t, rt, "acme", runID, "asked too soon", "U1")
	w.work(t, rt, nil)

	out := w.result(t, rt, runID)
	if out.Stopped == nil {
		t.Fatalf("Output = %+v: the early request was lost", out)
	}
	if n := len(model.asked()); n != 0 {
		t.Errorf("the model was called %d times; a recorded stop ends the turn before it starts", n)
	}
	want := []string{"user: explain the outage", "assistant: [This turn was stopped before it finished.]"}
	if got := said(journal(t, w.threads, tg.key())); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
}

// A run that has already completed is not listening. Asking it to stop is not
// an error and changes nothing: not its events, not its record, not its
// thread.
func TestAStopAfterTheRunCompletedDoesNothing(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("The whole answer")
	close(exec.finish) // the turn completes without waiting
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	tg := tag("docs-bot", "evt-1", "explain")
	runID := w.accept(t, rt, tg)
	out := w.result(t, rt, runID)
	if out.Stopped != nil {
		t.Fatalf("the run was stopped before anyone asked: %+v", out)
	}
	events := fingerprint(w.bus.of(runID))
	entries := said(journal(t, w.threads, tg.key()))

	w.stop(t, rt, "acme", runID, "too late", "U1")
	time.Sleep(50 * time.Millisecond)

	if got := fingerprint(w.bus.of(runID)); !reflect.DeepEqual(got, events) {
		t.Errorf("a stop after completion changed the run's events:\n got %v\nwant %v", got, events)
	}
	if got := said(journal(t, w.threads, tg.key())); !reflect.DeepEqual(got, entries) {
		t.Errorf("a stop after completion changed the thread:\n got %q\nwant %q", got, entries)
	}
	again := w.result(t, rt, runID)
	if again.Stopped != nil || again.Text != out.Text {
		t.Errorf("a stop after completion changed the run's result: %+v", again)
	}
	if n := exec.count(); n != 1 {
		t.Errorf("the executor ran %d times, want 1", n)
	}
}

// Stopping twice is stopping once. The first request the run hears is the one
// it honours; the second, whenever it lands, adds no event, no journal entry
// and no second reason.
func TestStoppingTwiceIsStoppingOnce(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ")
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	tg := tag("docs-bot", "evt-1", "explain")
	runID := w.accept(t, rt, tg)
	exec.inFlight(t)
	w.stop(t, rt, "acme", runID, "first", "U1")
	w.stop(t, rt, "acme", runID, "second", "U2")

	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "first" || out.Stopped.By != "U1" {
		t.Fatalf("Output.Stopped = %+v, want the first request", out.Stopped)
	}
	w.stop(t, rt, "acme", runID, "third", "U3")
	time.Sleep(50 * time.Millisecond)

	last := ended(t, w.bus.of(runID))
	if body := stoppedBody(t, last); body.Reason != "first" {
		t.Errorf("lifecycle.stopped carries reason %q, want the first", body.Reason)
	}
	want := []string{"user: explain", "assistant: Half an\n\n[This turn was stopped before it finished.]"}
	if got := said(journal(t, w.threads, tg.key())); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
}

// The process asked to stop a run is usually not the one executing it. Two
// runtimes share a ledger, a queue and a bus; only the second has a worker.
// The request is made on the first and the run, executing on the second,
// stops.
func TestAStopReachesARunExecutingOnAnotherWorker(t *testing.T) {
	w := newWorld(t)
	front := newPatient("never runs here")
	back := newPatient("Half an ")
	edge := w.runtime(t, front)
	executing := w.runtime(t, back)
	w.work(t, executing, nil)

	runID := w.accept(t, edge, tag("docs-bot", "evt-1", "explain"))
	back.inFlight(t)
	w.stop(t, edge, "acme", runID, "Stopped by a user", "slack:U1")

	out := w.result(t, edge, runID)
	if out.Stopped == nil || out.Stopped.Reason != "Stopped by a user" {
		t.Fatalf("Output.Stopped = %+v, want the request made on the other runtime", out.Stopped)
	}
	ended(t, w.bus.of(runID))
	if n := front.count(); n != 0 {
		t.Errorf("the runtime with no worker executed the run %d times", n)
	}
}

// Replay. The stopped event fails to publish once, so the turn is attempted
// again. The agent's stopped outcome is a record by then: the executor is not
// asked a second time, the journal is offered the identical bytes and keeps
// one copy, and the run ends stopped on the second attempt.
func TestReplayingAStoppedRunOffersTheJournalTheSameBytesAndRunsNothingTwice(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ", "answer")
	rt := w.runtime(t, exec)

	var once sync.Once
	w.bus.rec.failWhen(func(e event.Envelope) error {
		var err error
		if e.Type == event.TypeStopped {
			once.Do(func() { err = errors.New("broker outage") })
		}
		return err
	})
	w.work(t, rt, nil)

	tg := tag("docs-bot", "evt-1", "explain")
	runID := w.accept(t, rt, tg)
	exec.inFlight(t)
	w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")

	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Attempt != 2 {
		t.Fatalf("Output = %+v, want a stop announced on the second attempt", out)
	}
	if n := exec.count(); n != 1 {
		t.Fatalf("the executor ran %d times, want 1: a replay must not run the agent again", n)
	}

	want := []string{"user: explain", "assistant: Half an answer\n\n[This turn was stopped before it finished.]"}
	offered := w.threads.offered()
	if len(offered) != 2 {
		t.Fatalf("the journal was offered the exchange %d times, want once per attempt", len(offered))
	}
	for i, got := range offered {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("attempt %d offered the journal\n     %q\nwant %q", i+1, got, want)
		}
	}
	if got := said(journal(t, w.threads, tg.key())); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}

	events := w.bus.of(runID)
	ended(t, events)
	wantKinds := []string{
		event.TypeAccepted, event.TypeStarted, event.TypeText, event.TypeText,
		event.TypeResumed, event.TypeStopped,
	}
	if got := kindsOf(events); !sameKinds(got, wantKinds) {
		t.Errorf("events = %v, want %v", got, wantKinds)
	}
}

// A stop may land while no attempt exists: the first one failed and the next
// has not started. The request waits on the bus and the next attempt stops as
// soon as it listens.
func TestAStopBetweenTwoAttemptsStopsTheNextOne(t *testing.T) {
	w := newWorld(t)
	exec := newPatient()
	exec.failures = 1
	rt := w.runtime(t, exec)

	tg := tag("docs-bot", "evt-1", "explain")
	var runID string
	var asked sync.Once
	// The worker calls this after an attempt has failed and before it
	// schedules the next, which is exactly the gap.
	w.work(t, rt, func() {
		asked.Do(func() { w.stop(t, rt, "acme", runID, "between attempts", "U1") })
	})
	// The run id is a function of the tag, so it is known before Accept makes
	// the run claimable.
	runID = tg.runID()
	if got := w.accept(t, rt, tg); got != runID {
		t.Fatalf("Accept returned run %s, want %s", got, runID)
	}

	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "between attempts" || out.Attempt != 2 {
		t.Fatalf("Output = %+v, want the second attempt stopped", out)
	}
	wantKinds := []string{
		event.TypeAccepted, event.TypeStarted, event.TypeFailed,
		event.TypeResumed, event.TypeStopped,
	}
	events := w.bus.of(runID)
	if got := kindsOf(events); !sameKinds(got, wantKinds) {
		t.Fatalf("events = %v, want %v", got, wantKinds)
	}
	if events[len(events)-1].Type != event.TypeStopped {
		t.Fatalf("something followed the stop: %v", kindsOf(events))
	}
}

// An executor that never looks at the stop signal still stops. It is cancelled
// once the grace period is over, and the run ends stopped rather than failed,
// with the text it had published: the request was to stop, and it has.
func TestATurnThatIgnoresAStopIsCancelledAndTheRunStillEndsStopped(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ", "answer")
	exec.deaf = true
	rt := w.runtime(t, exec, runtime.WithStopGrace(20*time.Millisecond))
	w.work(t, rt, nil)

	tg := tag("docs-bot", "evt-1", "explain")
	runID := w.accept(t, rt, tg)
	exec.inFlight(t)
	w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")

	out := w.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "Stopped by a user" {
		t.Fatalf("Output = %+v, want the run stopped", out)
	}
	if out.Text != "Half an answer" {
		t.Errorf("Output.Text = %q, want the text that had been published", out.Text)
	}
	ended(t, w.bus.of(runID))
	for _, e := range w.bus.of(runID) {
		if e.Type == event.TypeFailed {
			t.Fatalf("a cancelled stop was reported as a failure: %v", kindsOf(w.bus.of(runID)))
		}
	}
	if n := exec.count(); n != 1 {
		t.Errorf("the executor ran %d times, want 1", n)
	}
	want := []string{"user: explain", "assistant: Half an answer\n\n[This turn was stopped before it finished.]"}
	if got := said(journal(t, w.threads, tg.key())); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal:\n got %q\nwant %q", got, want)
	}
}

// Stopping a parent stops what it is waiting on. The lead has delegated to a
// helper and is blocked on its answer; the helper's model is mid-call. The
// lead's run ends stopped, and the helper's call is cancelled rather than left
// to finish an answer nobody will read.
func TestStoppingAParentCancelsTheSubTaskItIsWaitingOn(t *testing.T) {
	w := newWorld(t)
	w.specs.revise("helper", "Help.")
	// The spawn grant is on the stored version, so the pinned lookup sees it.
	w.specs.set(native.Spec{
		Name: "lead", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "Lead.",
		Access: native.Access{Spawn: []string{"helper"}},
	})

	helper := newHeldModel("Counting")
	lead := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("call-1", runtime.DelegateToolName("helper"), map[string]any{"text": "Count the failing tests"}),
		agenttest.TextResponse("never reached"),
	}}
	// One node: the helper runs on the node the lead's turn holds.
	exec := sandboxOf(t, w.specs, map[string]saigetypes.Provider{"lead": lead, "helper": helper}, runtime.WithReplicas(1))
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("lead", "evt-1", "how are the tests?"))
	select {
	case <-helper.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the helper never started")
	}
	w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")

	out := w.result(t, rt, runID)
	if out.Stopped == nil {
		t.Fatalf("Output = %+v, want the lead's run stopped", out)
	}
	ended(t, w.bus.of(runID))
	select {
	case <-helper.cancelled:
	case <-time.After(20 * time.Second):
		t.Fatal("the helper's model call was left running after its parent was stopped")
	}
	if n := lead.CallCount(); n != 1 {
		t.Errorf("the lead's model was called %d times, want 1: a stopped turn asks nothing more", n)
	}
}

// A bus that can be published to and not read from cannot carry a stop to
// anything, and the runtime says so instead of accepting requests nobody will
// hear. A request that names no run is refused before it is published.
func TestStopRefusesWhatItCannotCarry(t *testing.T) {
	h := newHarness(t, newExecutor("answer")) // its bus is publish-only
	err := h.rt.Stop(context.Background(), runtime.StopRequest{Tenant: "acme", RunID: "r1"})
	if !errors.Is(err, runtime.ErrStopUnavailable) {
		t.Fatalf("Stop on a publish-only bus = %v, want ErrStopUnavailable", err)
	}

	w := newWorld(t)
	rt := w.runtime(t, newPatient())
	for _, id := range []string{"", "   ", "has:colon", "has space"} {
		if err := rt.Stop(context.Background(), runtime.StopRequest{Tenant: "acme", RunID: id}); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("Stop(%q) = %v, want ErrInvalid", id, err)
		}
	}
	if n := len(w.bus.rec.all()); n != 0 {
		t.Errorf("%d events were published for requests that were refused", n)
	}
}

// What a stop request looks like on the bus: it is on the run's control topic,
// never the run's own, it carries the tenant it was made under, and its free
// text is bounded.
func TestAStopRequestIsPublishedToTheControlTopicUnderTheCallersTenant(t *testing.T) {
	w := newWorld(t)
	w.busOnly = true // with a ledger, a request for a run it does not hold is not published
	rt := w.runtime(t, newPatient())
	long := strings.Repeat("é", runtime.MaxStopText)
	w.stop(t, rt, "acme", "r0123", "  spread\nover   lines ", long)

	events := w.bus.rec.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want the request alone", len(events))
	}
	e := events[0]
	want, err := runtime.StopChannel("acme", "r0123")
	if err != nil {
		t.Fatal(err)
	}
	if e.Channel != want || !strings.HasPrefix(e.Channel, "control://acme/stop/") {
		t.Errorf("channel = %q, want %q", e.Channel, want)
	}
	if other, _ := runtime.StopChannel("globex", "r0123"); other == want {
		t.Error("two tenants' requests for one run id share a stream")
	}
	if !event.IsControl(e.Channel) || e.Type != event.TypeStop || e.Tenant != "acme" {
		t.Errorf("event = channel %s type %s tenant %q", e.Channel, e.Type, e.Tenant)
	}
	if e.Seq != 0 {
		t.Error("the request is sequenced; a consumer tracking sequences per run would confuse it with the run's own events")
	}
	body, err := decode[event.StopRequest](e)
	if err != nil {
		t.Fatal(err)
	}
	if body.RunID != "r0123" || body.Reason != "spread over lines" || body.At.IsZero() {
		t.Errorf("body = %+v", body)
	}
	if len(body.By) > runtime.MaxStopText || !strings.HasPrefix(long, body.By) || body.By == "" {
		t.Errorf("By is %d bytes, want at most %d and cut on a rune boundary", len(body.By), runtime.MaxStopText)
	}
	for _, r := range body.By {
		if r != 'é' {
			t.Fatalf("By was cut inside a rune: %q", body.By)
		}
	}
}

// The control namespace is not an agent. A tag addressed to it is refused,
// because a run by that name would publish into the stream requests travel on.
// No definition can take the name either: it breaks the name rule.
func TestATagForTheControlNamespaceIsRefused(t *testing.T) {
	w := newWorld(t)
	rt := w.runtime(t, newPatient())
	if _, err := accept(context.Background(), rt, tag("_control", "evt-1", "hello")); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("Accept for the control namespace = %v, want ErrInvalid", err)
	}
}
