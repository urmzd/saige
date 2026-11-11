package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable/replay"
	drun "github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/turn/native"
)

// lineageOf is the lineage an event carries.
func lineageOf(e event.Envelope) runtime.Lineage {
	return runtime.Lineage{Root: e.Root, ParentRun: e.ParentRun, ParentToolCall: e.ParentToolCall}
}

// stamped checks that every event carries want, and that the events include
// each of types.
func stamped(t *testing.T, events []event.Envelope, want runtime.Lineage, types ...string) {
	t.Helper()
	for _, e := range events {
		if got := lineageOf(e); got != want {
			t.Errorf("%s seq %d carries lineage %+v, want %+v", e.Type, e.Seq, got, want)
		}
	}
	seen := kindsOf(events)
	for _, typ := range types {
		if !slices.Contains(seen, typ) {
			t.Errorf("no %s among %v", typ, seen)
		}
	}
}

// A run that no run called is the root of its own tree: every event names it
// as the root and names no parent, and its durable input names no lineage,
// so it hashes as a run accepted before lineage existed.
func TestARootRunIsItsOwnRootOnEveryEvent(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("done", event.Text{Text: "done"}))
	h.specs.revise("docs-bot", "v1")
	h.work(t)
	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-root", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}
	want := runtime.Lineage{Root: acc.RunID}
	stamped(t, h.bus.all(), want, event.TypeAccepted, event.TypeStarted, event.TypeText, event.TypeCompleted)
	if seen := h.exec.seen(); len(seen) != 1 || seen[0].Lineage != want {
		t.Errorf("the turn was handed %+v, want lineage %+v", seen, want)
	}
	held, _, err := h.ledger.Load(ctx, acc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(held.Input, []byte(`"lineage"`)) {
		t.Errorf("a root run's durable input names a lineage: %s", held.Input)
	}
}

// A run a host starts as a call of another run keeps that lineage in its
// durable input, so every attempt, and every replay of one, stamps the same
// lineage on every event, whatever the context it executes under. The
// journal reads the lineage back with the event.
func TestAReplayedAttemptStampsTheLineageTheRunWasAcceptedWith(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered", event.Text{Text: "ans"}, event.Text{Text: "wered"}))
	h.specs.revise("docs-bot", "v1")
	ctx := context.Background()
	want := runtime.Lineage{Root: "rroot", ParentRun: "rcaller", ParentToolCall: "htu_rcaller_3"}
	acc, err := h.rt.Accept(runtime.WithLineage(ctx, want), tag("docs-bot", "Ev-called", "hello").event(), "docs-bot")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	held, _, err := h.ledger.Load(ctx, acc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var in runtime.Input
	if err := json.Unmarshal(held.Input, &in); err != nil || in.Lineage != want {
		t.Fatalf("durable input holds lineage %+v (%v), want %+v", in.Lineage, err, want)
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
		if _, err := fn(replay.WithFrame(ctx, frame), held.Input); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	execute(1)
	first := len(h.bus.all())
	execute(1)
	execute(2)
	events := h.bus.all()
	if len(events) <= first {
		t.Fatal("the replays published nothing")
	}
	stamped(t, events, want, event.TypeAccepted, event.TypeStarted, event.TypeText, event.TypeResumed, event.TypeCompleted)
	if seen := h.exec.seen(); len(seen) != 1 || seen[0].Lineage != want {
		t.Errorf("the turn was handed %+v, want lineage %+v", seen, want)
	}
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		back, err := runtime.DecodeEntry(raw)
		if err != nil {
			t.Fatal(err)
		}
		if lineageOf(back) != want {
			t.Errorf("the journal read %s back with lineage %+v, want %+v", e.Type, lineageOf(back), want)
		}
	}
}

// A run a host admits as a call of a durable run is stamped with its lineage
// on every event, lifecycle included: accepted, started, parked, resumed
// with the answer in a later attempt, and completed. Root and parent come
// from the ledger's parent; the host passes the call.
func TestAHostChildIsStampedWithItsLineageAcrossAPark(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w,
		call("c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}),
		agenttest.TextResponse("Deployed."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)

	parent := p.accept(t, rt, tag("docs-bot", "evt-parent", "hello"))
	p.result(t, rt, parent)
	callID := "htu_" + parent + "_1"
	ctx := runtime.WithLineage(context.Background(), runtime.Lineage{ParentToolCall: callID})
	acc, err := rt.AcceptOwnChild(ctx, tag("ops", "evt-child", "ship").event(), "ops", parent)
	if err != nil {
		t.Fatalf("AcceptOwnChild: %v", err)
	}
	q := p.parkedOn(t, rt, acc.RunID)
	p.answer(t, rt, acc.RunID, q.ID, event.Answer{Choice: "prod"})
	if out := p.result(t, rt, acc.RunID); out.Text != "Deployed." {
		t.Fatalf("child Output = %+v", out)
	}

	want := runtime.Lineage{Root: parent, ParentRun: parent, ParentToolCall: callID}
	stamped(t, p.bus.of(acc.RunID), want,
		event.TypeAccepted, event.TypeStarted, event.TypeParked, event.TypeResumed, event.TypeCompleted)
	stamped(t, p.bus.of(parent), runtime.Lineage{Root: parent}, event.TypeAccepted, event.TypeCompleted)

	if again, err := rt.AcceptOwnChild(ctx, tag("ops", "evt-child", "ship").event(), "ops", parent); err != nil || again.RunID != acc.RunID {
		t.Errorf("a redelivered admission: %+v, %v", again, err)
	}
}

// A delegation this runtime starts as a child run is a call of its parent:
// the child names the parent as root and parent, and the call by the id the
// parent's own tool call events carry, so a subscriber nests the child under
// that call.
func TestADelegatedChildRunIsStampedAsItsParentsToolCall(t *testing.T) {
	w := newWorld(t)
	rt, p := childRunWorld(t, w)
	p.workers(t, rt, 2, 0)
	tg := tag("lead", "evt-1", "ship")
	tg.Actor = event.Actor{ID: "ana"}
	parent := p.accept(t, rt, tg)
	wait := pendingOf(t, rt, parent, event.QuestionDelegation)
	child := wait.ChildRunID
	q := pendingOf(t, rt, child, event.QuestionChoice)
	p.answer(t, rt, child, q.ID, event.Answer{Choice: "staging"})
	p.result(t, rt, parent)
	p.result(t, rt, child)

	if wait.Tool == nil || wait.Tool.CallID == "" {
		t.Fatalf("the delegation names no call: %+v", wait)
	}
	stamped(t, p.bus.of(parent), runtime.Lineage{Root: parent}, event.TypeParked, event.TypeResumed, event.TypeCompleted)
	stamped(t, p.bus.of(child), runtime.Lineage{Root: parent, ParentRun: parent, ParentToolCall: wait.Tool.CallID},
		event.TypeAccepted, event.TypeStarted, event.TypeParked, event.TypeResumed, event.TypeCompleted)

	var calls []string
	for _, e := range p.bus.of(parent) {
		if body, ok := e.Data.(event.ToolCall); ok {
			calls = append(calls, body.ID)
		}
	}
	if !slices.Contains(calls, wait.Tool.CallID) {
		t.Errorf("the child's call %q is not among the parent's tool calls %v", wait.Tool.CallID, calls)
	}
}

// A lineage that would nest a run under nothing, or a host child under
// another parent than the ledger's, is refused before anything is announced
// or started.
func TestALineageThatPlacesARunNowhereIsRefused(t *testing.T) {
	w := newWorld(t)
	rt := w.runtime(t, newExecutor("done"))
	ctx := context.Background()
	parent := w.accept(t, rt, tag("docs-bot", "evt-parent", "hello"))
	other := w.accept(t, rt, tag("docs-bot", "evt-other", "hello"))
	before := len(w.bus.rec.all())

	for i, l := range []runtime.Lineage{
		{ParentToolCall: "c1"},
		{Root: parent, ParentToolCall: "c1"},
		{ParentRun: parent, ParentToolCall: "c1"},
		{Root: parent},
	} {
		tg := tag("docs-bot", "evt-bad-"+string(rune('a'+i)), "hello")
		if _, err := rt.Accept(runtime.WithLineage(ctx, l), tg.event(), "docs-bot"); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("lineage %+v accepted: %v", l, err)
		}
		if _, _, err := w.ledger.Load(ctx, tg.runID()); !errors.Is(err, drun.ErrNotFound) {
			t.Errorf("lineage %+v left a run behind: %v", l, err)
		}
	}
	child := tag("docs-bot", "evt-child", "child").event()
	for _, l := range []runtime.Lineage{
		{Root: parent, ParentRun: other, ParentToolCall: "c1"},
		{Root: other, ParentRun: parent, ParentToolCall: "c1"},
	} {
		if _, err := rt.AcceptOwnChild(runtime.WithLineage(ctx, l), child, "docs-bot", parent); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("host child with lineage %+v under %s accepted: %v", l, parent, err)
		}
	}
	if after := len(w.bus.rec.all()); after != before {
		t.Errorf("refused runs published %d events", after-before)
	}
}

// A delegation the host admits (WithChildStarter) is asked for with the
// child's lineage on the context, so a host that accepts the child with that
// context stamps it as the parent's call without doing anything itself.
func TestAChildTheHostAdmitsIsStampedAsTheCallItWasAskedFor(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w)
	w.specs.set(native.Spec{
		Name: "lead", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "Lead.",
		Access: native.Access{Spawn: []string{"ops"}},
	})
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{
		"lead": routed{tool: runtime.DelegateToolName("ops"), args: map[string]any{"text": "deploy"}, done: "Lead got:"},
		"ops":  routed{tool: native.AskPersonToolName, args: map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}, done: "Child chose"},
	}, runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
		return native.StaticTools(p.deploy), nil
	}))
	var rt *runtime.Runtime
	asked := make(chan runtime.Lineage, 4)
	starter := func(ctx context.Context, parent runtime.Input, c runtime.SessionCall) (string, error) {
		asked <- runtime.LineageFrom(ctx)
		e := event.Envelope{
			ID: c.ID, Source: "delegation:" + parent.Run, Type: event.TypeMessagePosted, Channel: c.Session.Channel,
			Time: parent.Trigger.Time, Tenant: parent.Trigger.Tenant,
			Data: event.MessagePosted{Request: event.Request{Text: c.Text, Mentions: []string{c.Agent}}},
		}
		acc, err := rt.AcceptOwnSessionChild(ctx, e, c.Agent, c.Session, parent.Run, c.QuestionID)
		return acc.RunID, err
	}
	rt = p.rt(t, runtime.WithChildRuns(), runtime.WithSessions(session.NewMemory()),
		runtime.WithChildStarter(starter),
		runtime.WithChildRunID(func(parent runtime.Input, c runtime.SessionCall) string {
			return runtime.RunID(parent.Trigger.Tenant, c.ID)
		}))
	p.workers(t, rt, 2, 0)

	tg := tag("lead", "evt-1", "ship")
	tg.Actor = event.Actor{ID: "ana"}
	parent := p.accept(t, rt, tg)
	wait := pendingOf(t, rt, parent, event.QuestionDelegation)
	child := wait.ChildRunID
	q := pendingOf(t, rt, child, event.QuestionChoice)
	p.answer(t, rt, child, q.ID, event.Answer{Choice: "staging"})
	p.result(t, rt, parent)
	p.result(t, rt, child)

	want := runtime.Lineage{Root: parent, ParentRun: parent, ParentToolCall: wait.Tool.CallID}
	if got := <-asked; got != want {
		t.Errorf("the host was asked for a child with lineage %+v, want %+v", got, want)
	}
	stamped(t, p.bus.of(child), want, event.TypeAccepted, event.TypeStarted, event.TypeParked, event.TypeCompleted)
}
