package runtime_test

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/urmzd/saige/durable/replay"
	drun "github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/thread/threadtest"
	"github.com/urmzd/saige/flow/turn/native"
)

// ── fixtures ────────────────────────────────────────────────────────

// threadsEnv names the Postgres these tests keep their journal in. Without it
// the journal is in memory; with it, the runtime is exercised against the
// backend a deployment uses.
const threadsEnv = "SAIGE_TEST_POSTGRES_DSN"

var threadSchemas atomic.Uint64

// newThreads returns an empty thread store: Postgres in a schema of its own
// when $SAIGE_TEST_POSTGRES_DSN is set, Memory otherwise.
func newThreads(t *testing.T) thread.Store {
	t.Helper()
	url := os.Getenv(threadsEnv)
	if url == "" {
		return thread.NewMemory()
	}
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_runtime_%d_%d", time.Now().UnixNano(), threadSchemas.Add(1))
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse $%s: %v", threadsEnv, err)
	}
	cfg.MaxConns = 4
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
	store := thread.NewPostgres(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// exchange appends one earlier run's question and answer to a tag's thread,
// the way a completed run does.
func exchange(t *testing.T, s thread.Store, key thread.Key, run, question, answer string) {
	t.Helper()
	_, err := s.Append(context.Background(), key,
		thread.Entry{Role: thread.RoleUser, Text: question, RunID: run, Actor: event.Actor{ID: "U1"}},
		thread.Entry{Role: thread.RoleAssistant, Text: answer, RunID: run, Actor: event.Actor{ID: key.Agent, Bot: true}},
	)
	if err != nil {
		t.Fatalf("seed thread %s: %v", key, err)
	}
}

func journal(t *testing.T, s thread.Store, key thread.Key) []thread.Entry {
	t.Helper()
	head, err := s.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("Head(%s): %v", key, err)
	}
	out, err := s.History(context.Background(), key, head)
	if err != nil {
		t.Fatalf("History(%s): %v", key, err)
	}
	return out
}

func said(entries []thread.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = string(e.Role) + ": " + e.Text
	}
	return out
}

func replayed(history []native.Message) []string {
	out := make([]string, len(history))
	for i, m := range history {
		out[i] = string(m.Role) + ": " + m.Text
	}
	return out
}

// finish accepts a tag, lets the worker run it, and returns what the executor
// was handed for it.
func finish(t *testing.T, h *harness, tg fixture) runtime.Request {
	t.Helper()
	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tg)
	if err != nil {
		t.Fatalf("Accept(%s): %v", tg.ID, err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result(%s): %v", tg.ID, err)
	}
	for _, req := range h.exec.seen() {
		if req.RunID == acc.RunID {
			return req
		}
	}
	t.Fatalf("the executor never saw run %s", acc.RunID)
	return runtime.Request{}
}

// ── tests ───────────────────────────────────────────────────────────

// The reason the package exists: the second tag in a thread is answered by an
// agent that was shown the first exchange, the third by one shown both, in the
// order they happened, and what it is shown is a role and a text and nothing
// that differs from one run to the next.
func TestARunIsShownTheConversationSoFarInItsThread(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	exec := newExecutor("")
	exec.prompt = func(req runtime.Request) string { return "asked: " + req.Text }
	h := newHarness(t, exec, runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	var want []string
	for k := 1; k <= 3; k++ {
		exec.mu.Lock()
		exec.answer = fmt.Sprintf("answer %d", k)
		exec.mu.Unlock()

		req := finish(t, h, tag("docs-bot", fmt.Sprintf("Ev-thread-%d", k), fmt.Sprintf("question %d", k)))
		if got := replayed(req.History); !reflect.DeepEqual(got, append([]string{}, want...)) {
			t.Fatalf("run %d was shown %q, want %q", k, got, want)
		}
		for _, m := range req.History {
			for _, volatile := range []string{"Ev-thread-", "2026-"} {
				if strings.Contains(m.Text, volatile) {
					t.Errorf("run %d: replayed history carries %q, which differs between runs: %q", k, volatile, m.Text)
				}
			}
		}
		want = append(want, fmt.Sprintf("user: asked: question %d", k), fmt.Sprintf("assistant: answer %d", k))
	}

	// The journal is the record: what the model was asked, then what it
	// answered, attributed to who said it and the run that produced it.
	got := journal(t, store, tag("docs-bot", "any", "").key())
	if !reflect.DeepEqual(said(got), want) {
		t.Fatalf("journal = %q, want %q", said(got), want)
	}
	if e := got[2]; e.RunID != runtime.RunKey("acme", "Ev-thread-2", "docs-bot") || e.Seq != 3 || !e.At.Equal(tag("docs-bot", "x", "").At) {
		t.Errorf("the second question was journaled as %+v", e)
	}
	if e := got[3]; e.RunID != runtime.RunKey("acme", "Ev-thread-2", "docs-bot") || !e.Actor.Bot || e.Actor.ID != "docs-bot" {
		t.Errorf("the second answer was journaled as %+v", e)
	}
}

// THE property this change has to protect, the counterpart of revision
// pinning: a run pins its history at accept time and is shown exactly that,
// even though the thread kept growing while the run sat in the queue.
func TestARunPinnedToAHistoryIsShownExactlyThatAfterTheThreadGrows(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	h := newHarness(t, newExecutor("answered"), runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	tg := tag("docs-bot", "Ev-pin", "what did we decide?")
	key := tg.key()
	exchange(t, store, key, "run-a", "first question", "first answer")

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tg)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	body, _, err := h.ledger.Load(ctx, acc.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(string(body.Input), `"history_seq":2`) {
		t.Fatalf("the run's durable input does not pin the history at 2: %s", body.Input)
	}

	// The thread grows while the run sits in the queue, which is exactly the
	// race a busy thread has: a tag arrives, two more people tag the agent,
	// and the worker picks the first run up afterwards.
	exchange(t, store, key, "run-b", "second question", "second answer")
	exchange(t, store, key, "run-c", "third question", "third answer")

	h.work(t)
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}
	seen := h.exec.seen()
	if len(seen) != 1 {
		t.Fatalf("the executor ran %d times, want 1", len(seen))
	}
	if got, want := replayed(seen[0].History), []string{"user: first question", "assistant: first answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the run was shown %q, want exactly the history it pinned, %q", got, want)
	}
	// What the run adds lands after everything, whenever it was pinned.
	if got := said(journal(t, store, key)); len(got) != 8 || got[6] != "user: what did we decide?" || got[7] != "assistant: answered" {
		t.Fatalf("journal = %q, want the run's exchange appended at the end", got)
	}
}

// durable re-executes a run from the top on every attempt. The history must
// be read from the pin, never from "latest", or the second attempt asks the
// model a different question than the first one did.
func TestEveryAttemptOfARunIsShownTheSameHistory(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	exec := newExecutor("third time lucky")
	h := newHarness(t, exec, runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	tg := tag("docs-bot", "Ev-attempts", "hello")
	key := tg.key()
	exchange(t, store, key, "run-a", "first question", "first answer")

	// Every attempt finds the thread longer than the attempt before it did:
	// someone else's run completes in the gap.
	var n atomic.Int64
	exec.hook = func(runtime.Request) {
		run := fmt.Sprintf("run-between-%d", n.Add(1))
		exchange(t, store, key, run, "asked in between", "answered in between")
	}
	exec.failNext(2, errors.New("provider timed out"))
	h.work(t)

	req := finish(t, h, tg)
	seen := h.exec.seen()
	if len(seen) != 3 {
		t.Fatalf("the executor ran %d times, want 3 attempts", len(seen))
	}
	want := []string{"user: first question", "assistant: first answer"}
	for i, attempt := range seen {
		if attempt.Attempt != i+1 {
			t.Errorf("execution %d reports attempt %d", i+1, attempt.Attempt)
		}
		if got := replayed(attempt.History); !reflect.DeepEqual(got, want) {
			t.Errorf("attempt %d was shown %q, want the pinned %q", i+1, got, want)
		}
	}
	if !reflect.DeepEqual(req.History, seen[0].History) {
		t.Errorf("the attempt that succeeded saw a different history than the first")
	}
}

// A redelivery joins the run it duplicated and cannot re-pin it: the history
// a run is shown is whatever the thread held the first time its tag arrived.
func TestARedeliveredTagCannotRepinItsHistory(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	h := newHarness(t, newExecutor("answered"), runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	tg := tag("docs-bot", "Ev-redeliver", "hello")
	key := tg.key()
	exchange(t, store, key, "run-a", "first question", "first answer")

	ctx := context.Background()
	first, err := accept(ctx, h.rt, tg)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	exchange(t, store, key, "run-b", "second question", "second answer")
	second, err := accept(ctx, h.rt, tg)
	if err != nil {
		t.Fatalf("Accept (redelivery): %v", err)
	}
	if second.RunID != first.RunID {
		t.Fatalf("the redelivery started run %s, want it to join %s", second.RunID, first.RunID)
	}

	h.work(t)
	if _, err := h.rt.Result(ctx, first.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}
	seen := h.exec.seen()
	if len(seen) != 1 {
		t.Fatalf("the executor ran %d times, want 1", len(seen))
	}
	if got := replayed(seen[0].History); len(got) != 2 {
		t.Fatalf("the run was shown %q: the redelivery re-pinned its history", got)
	}
}

// Replay must not write the conversation twice. The turn body re-executes on
// every replay and on every later attempt, and the journal must end up exactly
// as if it had run once.
func TestACompletedRunAppendsItsExchangeExactlyOnce(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	exec := newExecutor("answered")
	exec.prompt = func(req runtime.Request) string { return "Context from the trigger:\n- pr: 7\n\n" + req.Text }
	h := newHarness(t, exec,
		runtime.WithThreads(store),
		runtime.WithMessageID(func(e event.Envelope) string { return contextOf(e, "slack_ts") }),
	)
	h.specs.revise("docs-bot", "v1")
	tg := tag("docs-bot", "Ev-once", "hello")
	tg.Meta = map[string]string{"slack_ts": "1699.042"}
	tg.Actor = event.Actor{ID: "U1", Display: "ada"}
	key := tg.key()

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tg)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	body, _, err := h.ledger.Load(ctx, acc.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fn, ok := h.rt.Registry().Lookup(runtime.ActivityTurn)
	if !ok {
		t.Fatalf("activity %q is not registered", runtime.ActivityTurn)
	}
	execute := func(attempt int) {
		t.Helper()
		_, records, err := h.ledger.Load(ctx, acc.RunID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		frame := replay.NewFrame(acc.RunID, attempt, h.ledger, drun.JSONCodec{}, records, nil)
		if _, err := fn(replay.WithFrame(ctx, frame), body.Input); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}

	execute(1)
	first := journal(t, store, key)
	if got, want := said(first), []string{"user: Context from the trigger:\n- pr: 7\n\nhello", "assistant: answered"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("journal after the run = %q, want %q", got, want)
	}
	if e := first[0]; e.RunID != acc.RunID || e.SurfaceID != "1699.042" || e.Actor != tg.Actor {
		t.Errorf("the request was journaled without its run, its surface message or its author: %+v", e)
	}

	execute(1) // the same attempt, replayed from the ledger
	execute(2) // a later attempt, keeping the memo
	if n := h.exec.count(); n != 1 {
		t.Fatalf("the executor ran %d times, want 1", n)
	}
	if again := journal(t, store, key); !reflect.DeepEqual(again, first) {
		t.Fatalf("a replay changed the journal:\n got %+v\nwant %+v", again, first)
	}
}

// A thread is a tenant, an agent and a surface. Change any one and it is a
// different conversation with nothing carried over, and a tag with no surface
// continues nothing at all.
func TestHistoryIsNotSharedAcrossTenantAgentOrSurface(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	h := newHarness(t, newExecutor("answered"), runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	h.specs.revise("review-bot", "v1")
	h.work(t)

	finish(t, h, tag("docs-bot", "Ev-base", "the base question"))
	if got := finish(t, h, tag("docs-bot", "Ev-same", "again")).History; len(got) != 2 {
		t.Fatalf("a second tag in the same thread was shown %q, want the first exchange", replayed(got))
	}

	otherAgent := tag("review-bot", "Ev-agent", "hello")
	otherTenant := tag("docs-bot", "Ev-tenant", "hello")
	otherTenant.Tenant = "globex"
	otherThread := tag("docs-bot", "Ev-surface", "hello")
	otherThread.Source = address.MustParse("github://urmzd/fiber/issues/43")
	noSurface := tag("docs-bot", "Ev-cron-1", "nightly")
	noSurface.Source = address.Address{}
	noSurfaceAgain := tag("docs-bot", "Ev-cron-2", "nightly")
	noSurfaceAgain.Source = address.Address{}

	for name, tg := range map[string]fixture{
		"another agent in the same surface thread": otherAgent,
		"another tenant":               otherTenant,
		"another surface":              otherThread,
		"a tag with no surface":        noSurface,
		"the next tag with no surface": noSurfaceAgain,
	} {
		if got := finish(t, h, tg).History; len(got) != 0 {
			t.Errorf("%s was shown %q, want nothing", name, replayed(got))
		}
	}

	// The same pull request after a push is still the same conversation.
	pushed := tag("docs-bot", "Ev-push", "after the push")
	pushed.Source = tag("docs-bot", "x", "").Source.WithParam("head_sha", "abc123")
	if got := finish(t, h, pushed).History; len(got) != 4 {
		t.Errorf("a tag on the same surface with a new head_sha was shown %d messages, want the 4 already said", len(got))
	}
	// A one-shot thread is still recorded, under its own tag.
	if got := said(journal(t, store, noSurface.key())); !reflect.DeepEqual(got, []string{"user: nightly", "assistant: answered"}) {
		t.Errorf("a one-shot thread's journal = %q", got)
	}
}

// Two tags in one thread at once. Each keeps the pin it took at accept, and
// each run's question stays directly followed by its own answer: sequence
// numbers never interleave and no entry is lost.
func TestTagsRaisedTogetherInOneThreadKeepTheirExchangesWhole(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	exec := newExecutor("")
	exec.prompt = func(req runtime.Request) string { return "q " + req.RunID }
	h := newHarness(t, exec, runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	exec.mu.Lock()
	exec.answer = "an answer"
	exec.mu.Unlock()

	const tags = 12
	ctx := context.Background()
	ids := make([]string, tags)
	for i := range ids {
		acc, err := accept(ctx, h.rt, tag("docs-bot", fmt.Sprintf("Ev-together-%02d", i), "hello"))
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		ids[i] = acc.RunID
	}

	// Several workers, so the runs really do complete at once.
	wctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = worker.New(h.ledger, h.queue,
			worker.WithRegistry(h.rt.Registry()),
			worker.WithConcurrency(6),
			worker.WithJanitorEvery(0),
			worker.WithLeaseTTL(5*time.Second),
			worker.WithBackoff(func(int) time.Duration { return time.Millisecond }),
		).Run(wctx)
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	for _, id := range ids {
		if _, err := h.rt.Result(ctx, id); err != nil {
			t.Fatalf("Result(%s): %v", id, err)
		}
	}

	for _, req := range h.exec.seen() {
		if len(req.History) != 0 {
			t.Errorf("run %s was accepted into an empty thread and was shown %q", req.RunID, replayed(req.History))
		}
	}
	got := journal(t, store, tag("docs-bot", "x", "").key())
	if len(got) != tags*2 {
		t.Fatalf("journal holds %d entries, want %d: an entry was lost or written twice", len(got), tags*2)
	}
	answered := map[string]bool{}
	for i := 0; i < len(got); i += 2 {
		q, a := got[i], got[i+1]
		if q.Seq != uint64(i+1) || a.Seq != uint64(i+2) {
			t.Fatalf("entries %d and %d have seqs %d and %d", i, i+1, q.Seq, a.Seq)
		}
		if q.Role != thread.RoleUser || a.Role != thread.RoleAssistant || q.RunID != a.RunID || answered[q.RunID] {
			t.Fatalf("entries %d and %d are %s of %s and %s of %s: two runs interleaved", i, i+1, q.Role, q.RunID, a.Role, a.RunID)
		}
		answered[q.RunID] = true
	}
}

// With no thread store a run is exactly what it was before threads existed:
// nothing is pinned, nothing is replayed, and its durable input is the same
// bytes, so runs already in a ledger still match their records.
func TestWithoutAThreadStoreARunIsWhatItAlwaysWas(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered"))
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	ctx := context.Background()
	for _, id := range []string{"Ev-plain-1", "Ev-plain-2"} {
		req := finish(t, h, tag("docs-bot", id, "hello"))
		if req.History != nil {
			t.Errorf("%s was shown a history with no thread store: %q", id, replayed(req.History))
		}
		body, _, err := h.ledger.Load(ctx, runtime.RunKey("acme", id, "docs-bot"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if strings.Contains(string(body.Input), "history") {
			t.Errorf("%s: the durable input changed shape with no thread store: %s", id, body.Input)
		}
	}
}

// lossy is a journal that can forget, which no real one may. It exists so a
// test can prove that flow notices.
type lossy struct {
	mu sync.Mutex
	thread.Store
	appendErr error
	appendN   int
}

func (l *lossy) forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Store = thread.NewMemory()
}

func (l *lossy) inner() thread.Store {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Store
}

func (l *lossy) Head(ctx context.Context, key thread.Key) (uint64, error) {
	return l.inner().Head(ctx, key)
}

func (l *lossy) History(ctx context.Context, key thread.Key, upTo uint64) ([]thread.Entry, error) {
	return l.inner().History(ctx, key, upTo)
}

func (l *lossy) Append(ctx context.Context, key thread.Key, entries ...thread.Entry) ([]thread.Entry, error) {
	l.mu.Lock()
	if l.appendN > 0 {
		l.appendN--
		err := l.appendErr
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.inner().Append(ctx, key, entries...)
}

// A run pinned to a history the journal can no longer produce fails
// terminally and before the model is called, like a pinned revision whose
// hash moved. Answering from a shorter history would be an agent that forgot
// without saying so.
func TestAJournalThatLostWhatARunPinnedFailsTheRunTerminally(t *testing.T) {
	t.Parallel()

	store := &lossy{Store: thread.NewMemory()}
	h := newHarness(t, newExecutor("answered"), runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	tg := tag("docs-bot", "Ev-lost", "hello")
	exchange(t, store, tg.key(), "run-a", "first question", "first answer")

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tg)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	store.forget()
	h.work(t)

	if _, err := h.rt.Result(ctx, acc.RunID); err == nil {
		t.Fatal("the run succeeded against a journal that lost its history")
	}
	if n := h.exec.count(); n != 0 {
		t.Errorf("the executor ran %d times, want 0: the loss must be caught before the model", n)
	}
	failed := h.bus.ofKind(event.TypeFailed)
	if len(failed) == 0 {
		t.Fatal("no failure was published")
	}
	body, err := decode[lifecycle](failed[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Terminal {
		t.Error("a lost history was reported as retryable")
	}
	if !strings.Contains(body.Error, "missing entries") {
		t.Errorf("failure reports %q, want it to name the gap", body.Error)
	}
}

// A journal that is briefly unavailable when a run completes must not cost
// the thread its memory or the deployment a second model call: the attempt
// fails, the retry replays the memoized answer, and the exchange is recorded.
func TestAFailedJournalAppendIsRetriedWithoutPayingForTheModelAgain(t *testing.T) {
	t.Parallel()

	store := &lossy{Store: thread.NewMemory(), appendErr: errors.New("journal unavailable"), appendN: 1}
	h := newHarness(t, newExecutor("answered"), runtime.WithThreads(store))
	h.specs.revise("docs-bot", "v1")
	h.work(t)
	tg := tag("docs-bot", "Ev-append", "hello")

	finish(t, h, tg)
	if n := h.exec.count(); n != 1 {
		t.Errorf("the executor ran %d times, want 1: the retry must reuse the memoized answer", n)
	}
	if got := said(journal(t, store, tg.key())); !reflect.DeepEqual(got, []string{"user: hello", "assistant: answered"}) {
		t.Fatalf("journal = %q, want the exchange recorded by the retry", got)
	}
	failed := h.bus.ofKind(event.TypeFailed)
	if len(failed) != 1 {
		t.Fatalf("got %d failed events, want 1 for the attempt that could not record", len(failed))
	}
	if body, err := decode[lifecycle](failed[0]); err != nil || body.Terminal {
		t.Errorf("the failed append was reported as %+v, %v; want a retryable failure", body, err)
	}
	completed := h.bus.ofKind(event.TypeCompleted)
	if len(completed) != 1 || completed[0].Seq <= failed[0].Seq {
		t.Errorf("want exactly one completed event, after the failed attempt; got %d", len(completed))
	}
}

// A surface is the real record of a thread. Before a run's history is pinned
// the journal is brought up to date with it, so the run is shown what people
// said without tagging the agent, and is not shown its own request twice.
func TestTheThreadIsReconciledWithItsSurfaceBeforeHistoryIsPinned(t *testing.T) {
	t.Parallel()

	store := newThreads(t)
	surface := threadtest.NewSurface()
	h := newHarness(t, newExecutor("try make rollback"),
		runtime.WithThreads(store),
		runtime.WithReconciler(surface),
		// The surface's message id is the event's id, as Slack's ts is.
		runtime.WithMessageID(func(e event.Envelope) string { return e.ID }),
	)
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	raise := func(who, text string) fixture {
		tg := tag("docs-bot", "", text)
		tg.Actor = event.Actor{ID: who, Display: who}
		tg.ID = surface.Post(tg.Tenant, tg.Source, who, "@docs-bot "+text)
		return tg
	}
	source := tag("docs-bot", "x", "").Source

	surface.Post("acme", source, "ada", "the deploy failed")
	surface.Post("acme", source, "grace", "at the migrate stage")
	first := raise("ada", "what should we do?")
	req := finish(t, h, first)
	if got, want := replayed(req.History), []string{"user: ada: the deploy failed", "user: grace: at the migrate stage"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the first run was shown %q, want what was said before it, %q", got, want)
	}

	// The agent's answer is rendered onto the surface, someone edits what
	// they said earlier, someone else replies without tagging anyone, and
	// then the agent is tagged again.
	surface.PostOwn("acme", source, "try make rollback")
	late := surface.Post("acme", source, "grace", "that worked")
	second := raise("grace", "why did it fail?")
	req = finish(t, h, second)
	want := []string{
		"user: ada: the deploy failed",
		"user: grace: at the migrate stage",
		"user: what should we do?",
		"assistant: try make rollback",
		"user: grace: that worked",
	}
	if got := replayed(req.History); !reflect.DeepEqual(got, want) {
		t.Fatalf("the second run was shown %q, want %q", got, want)
	}

	// A message posted after the tag was accepted is not in that run's
	// history, and an edit or a delete rewrites nothing.
	before := journal(t, store, first.key())
	surface.Edit("acme", source, late, "that did not work after all")
	surface.Delete("acme", source, first.Meta["message_id"])
	third := raise("ada", "anything else?")
	req = finish(t, h, third)
	if got := replayed(req.History); !reflect.DeepEqual(got[:len(want)], want) || len(got) != len(want)+2 {
		t.Fatalf("after an edit and a delete the third run was shown %q, want the earlier history unchanged plus the second exchange", got)
	}
	if after := journal(t, store, first.key()); !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("an edit or delete on the surface rewrote the journal:\n got %+v\nwant %+v", after[:len(before)], before)
	}

	// A surface that cannot be listed does not refuse the tag. The run pins
	// what the journal already holds.
	surface.Fail(errors.New("slack is down"))
	held := len(journal(t, store, first.key()))
	fourth := tag("docs-bot", "Ev-rec-4", "still there?")
	if got := finish(t, h, fourth).History; len(got) != held {
		t.Fatalf("with the surface down the run was shown %d messages, want the %d the journal holds", len(got), held)
	}
}

// End to end through the real sandbox: the pinned history reaches the model
// as messages between the system prompt and the request, the request comes
// back as the prompt the journal records, and an agent the turn delegates to
// is shown none of the parent's thread.
func TestTheSandboxReplaysHistoryToTheModelAndNotToADelegatedAgent(t *testing.T) {
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	history := newSpecs()
	history.revise("helper", "Help.")
	history.revise("lead", "Lead.")
	lead := history.edit("lead", func(s *native.Spec) { s.Access.Spawn = []string{"helper"} })

	var mu sync.Mutex
	models := map[string]*agenttest.ScriptedProvider{}
	scripts := map[string][][]saigetypes.Delta{
		"lead": {
			agenttest.ToolCallResponse("call-1", runtime.DelegateToolName("helper"), map[string]any{"text": "Count the failing tests"}),
			agenttest.TextResponse("The helper finished."),
		},
		"helper": {agenttest.TextResponse("Three tests fail.")},
	}
	s, err := runtime.NewSandbox(plane, tools, history, runtime.WithReplicas(1),
		runtime.WithAgentOptions(native.WithProvider(native.ProviderOffline, func(spec native.Spec) (saigetypes.Provider, error) {
			mu.Lock()
			defer mu.Unlock()
			p := &agenttest.ScriptedProvider{Responses: scripts[spec.Name]}
			models[spec.Name] = p
			return p, nil
		})))
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.Execute(context.Background(), withPin(runtime.Request{
		RunID: "run-1", Tenant: "acme", Attempt: 1, Text: "How are the tests?",
		History: []native.Message{
			{Role: native.RoleUser, Text: "SECRET-OF-THE-THREAD: we ship on friday"},
			{Role: native.RoleAssistant, Text: "noted"},
		},
	}, lead), func(context.Context, event.Body) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out.Prompt != "How are the tests?" {
		t.Errorf("Outcome.Prompt = %q, want the request as the model was sent it", out.Prompt)
	}

	mu.Lock()
	defer mu.Unlock()
	leadCalls := models["lead"].Requests()
	if len(leadCalls) == 0 {
		t.Fatal("the lead's model was never called")
	}
	msgs := leadCalls[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("the lead's first request has %d messages, want the system prompt, two history messages and the request", len(msgs))
	}
	if _, ok := msgs[1].(saigetypes.UserMessage); !ok {
		t.Errorf("history message 1 reached the model as %T", msgs[1])
	}
	if _, ok := msgs[2].(saigetypes.AssistantMessage); !ok {
		t.Errorf("history message 2 reached the model as %T", msgs[2])
	}
	if rendered := fmt.Sprintf("%+v", msgs[1:3]); !strings.Contains(rendered, "SECRET-OF-THE-THREAD") {
		t.Errorf("the lead was not shown its thread: %s", rendered)
	}

	helperCalls := models["helper"].Requests()
	if len(helperCalls) != 1 {
		t.Fatalf("the helper's model was called %d times, want 1", len(helperCalls))
	}
	if n := len(helperCalls[0].Messages); n != 2 {
		t.Errorf("the delegated agent's request has %d messages, want only its system prompt and the delegation", n)
	}
	if rendered := fmt.Sprintf("%+v", helperCalls[0].Messages); strings.Contains(rendered, "SECRET-OF-THE-THREAD") {
		t.Errorf("the delegated agent was shown the parent's thread: %s", rendered)
	}
}
