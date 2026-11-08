package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable"
	drun "github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn/native"
)

// ── fixtures ────────────────────────────────────────────────────────

// pricedModel is a scripted model that declares what saige's catalog would
// for a real one: a rate card with its date, and a context window.
type pricedModel struct {
	*agenttest.ScriptedProvider
}

func (pricedModel) Capabilities() saigetypes.ModelCapabilities {
	return saigetypes.ModelCapabilities{
		Model: "claude-test", ContextWindow: 200_000, Known: true,
		Pricing: saigetypes.Pricing{InputPerMTok: 3, OutputPerMTok: 15, CachedInputPerMTok: 0.3, AsOf: "2026-07-24"},
	}
}

func priced(responses ...[]saigetypes.Delta) pricedModel {
	return pricedModel{&agenttest.ScriptedProvider{Responses: responses}}
}

// spending is a scripted response that reports what its call spent.
func spending(deltas []saigetypes.Delta, in, out int) []saigetypes.Delta {
	return append(append([]saigetypes.Delta(nil), deltas...), saigetypes.UsageDelta{PromptTokens: in, CompletionTokens: out})
}

// usageOf returns the usage.call bodies a run published, in order.
func usageOf(t *testing.T, events []event.Envelope) []event.UsageCall {
	t.Helper()
	var out []event.UsageCall
	for _, e := range events {
		if e.Type != event.TypeUsageCall {
			continue
		}
		c, err := decode[event.UsageCall](e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// tallied is what a subscriber rebuilds from a run's usage events.
func tallied(t *testing.T, events []event.Envelope) event.RunUsage {
	t.Helper()
	var tally event.Tally
	for _, e := range events {
		switch e.Type {
		case event.TypeUsageCall:
			c, err := decode[event.UsageCall](e)
			if err != nil {
				t.Fatal(err)
			}
			tally.Call(c)
		case event.TypeUsageChild:
			c, err := decode[event.UsageChild](e)
			if err != nil {
				t.Fatal(err)
			}
			tally.Child(c)
		}
	}
	return tally.Usage()
}

func sameUsage(a, b event.Usage) bool { return reflect.DeepEqual(a, b) }

func usageRecords(t *testing.T, w *world, runID string) []string {
	t.Helper()
	_, records, err := w.ledger.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("Load(%s): %v", runID, err)
	}
	var keys []string
	for _, rec := range records {
		if strings.HasPrefix(rec.Key, "event:fiber.usage") {
			keys = append(keys, rec.Key)
		}
	}
	// The in-memory ledger returns a run's records in map order.
	sort.Strings(keys)
	return keys
}

// spender is an executor that reports one model call per execution, keyed as
// the sandbox keys them, and fails the first fail executions after the call
// was made: a provider that took the tokens and then broke.
type spender struct {
	fail      int
	permanent bool

	mu   sync.Mutex
	runs int
}

func (s *spender) Execute(ctx context.Context, req runtime.Request, emit func(context.Context, event.Body) error) (runtime.Outcome, error) {
	s.mu.Lock()
	s.runs++
	n := s.runs
	s.mu.Unlock()

	body := event.UsageCall{
		AccountingID: req.RunID + "/a" + strconv.Itoa(req.Attempt) + "/1",
		Agent:        req.Agent(), Model: "claude-test", Requests: 1,
		PromptTokens: 100 * n, CompletionTokens: 10 * n,
		Cost: &event.Cost{Micros: int64(1000 * n), Currency: "USD", AsOf: "2026-07-24"},
	}
	if err := emit(ctx, body); err != nil {
		return runtime.Outcome{}, err
	}
	if n <= s.fail {
		err := fmt.Errorf("provider unavailable after call %d", n)
		if s.permanent && n == s.fail {
			return runtime.Outcome{}, durable.NonRetryable(err)
		}
		return runtime.Outcome{}, err
	}
	return runtime.Outcome{Text: "answered", Prompt: req.Text}, nil
}

func (s *spender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs
}

// forgetful is a thread store whose first append fails: the agent has answered,
// and the run fails after it, so the next attempt replays the agent.
type forgetful struct {
	thread.Store
	mu     sync.Mutex
	failed bool
}

func (f *forgetful) Append(ctx context.Context, key thread.Key, entries ...thread.Entry) ([]thread.Entry, error) {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return nil, errors.New("journal unavailable")
	}
	return f.Store.Append(ctx, key, entries...)
}

// ── tests ───────────────────────────────────────────────────────────

// Through the real executor and agent loop: each model call is published as a
// usage.call carrying saige's figures, and the run's total on its completed
// event, its output and its ledger record is exactly those calls summed.
func TestTheRunsTotalIsTheSumOfItsCallEvents(t *testing.T) {
	w := newWorld(t)
	w.specs.revise("docs-bot", "You document things.")
	model := priced(
		spending(agenttest.TextResponse("The docs are updated."), 12_000, 300),
	)
	rt := w.runtime(t, sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model}))
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "update the docs"))
	out := w.result(t, rt, runID)
	events := w.bus.of(runID)

	calls := usageOf(t, events)
	if len(calls) != 1 {
		t.Fatalf("published %d usage.call events, want 1: %v", len(calls), kindsOf(events))
	}
	c := calls[0]
	if c.PromptTokens != 12_000 || c.CompletionTokens != 300 || c.Model != "claude-test" || c.ContextWindow != 200_000 {
		t.Errorf("usage.call = %+v, want saige's figures", c)
	}
	if c.Cost == nil || c.Cost.Micros != 40_500 || c.Cost.Currency != "USD" || c.Cost.AsOf != "2026-07-24" {
		t.Errorf("usage.call cost = %+v, want 12000 in at 3/Mtok and 300 out at 15/Mtok, USD 0.040500 as of 2026-07-24", c.Cost)
	}
	if !strings.HasPrefix(c.AccountingID, runID+"/a1/") {
		t.Errorf("AccountingID = %q, want it keyed under the run and attempt", c.AccountingID)
	}

	if out.Usage == nil {
		t.Fatal("Output.Usage is nil for a run that called a model")
	}
	sum := tallied(t, events)
	if !sameUsage(out.Usage.Total, sum.Total) || !sameUsage(out.Usage.Own, sum.Total) {
		t.Errorf("Output.Usage = %+v, want the call events summed %+v", out.Usage, sum)
	}
	if ctx := out.Usage.Context; ctx == nil || ctx.Window != 200_000 || ctx.Tokens != 12_000 {
		t.Errorf("Output.Usage.Context = %+v, want 12000 of 200000", ctx)
	}
	completed, err := decode[lifecycle](events[len(events)-1])
	if err != nil || events[len(events)-1].Type != event.TypeCompleted {
		t.Fatalf("the last event is %s (%v), want completed", events[len(events)-1].Type, err)
	}
	if completed.Usage == nil || !sameUsage(completed.Usage.Total, out.Usage.Total) {
		t.Errorf("lifecycle.completed usage = %+v, want the run's total", completed.Usage)
	}

	// The total is in the ledger, so a process that never ran the run reads it.
	after := w.restarted()
	again := after.result(t, after.runtime(t, newPatient()), runID)
	if again.Usage == nil || !sameUsage(again.Usage.Total, out.Usage.Total) {
		t.Errorf("after a restart Output.Usage = %+v, want %+v", again.Usage, out.Usage)
	}
}

// An attempt that fails after spending tokens did spend them, so the run's
// total counts them alongside the attempt that answered. A replay of the
// memoized agent spends nothing and adds nothing: here the journal append fails
// after the agent answered, the third attempt replays, and the total is still
// the two calls the model was paid for.
func TestARetriedAttemptsSpendIsCountedOnceAndAReplayAddsNothing(t *testing.T) {
	w := newWorld(t)
	exec := &spender{fail: 1}
	rt := w.runtime(t, exec, runtime.WithThreads(&forgetful{Store: w.threads}))
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	out := w.result(t, rt, runID)

	if out.Attempt != 3 {
		t.Fatalf("Output.Attempt = %d, want 3: a failed agent, a failed append, then a replay", out.Attempt)
	}
	if n := exec.count(); n != 2 {
		t.Fatalf("the executor ran %d times, want 2: the replay must not call the model", n)
	}
	if out.Usage == nil {
		t.Fatal("Output.Usage is nil")
	}
	total := out.Usage.Total
	if total.ModelCalls != 2 || total.PromptTokens != 300 || total.CompletionTokens != 30 || total.Cost == nil || total.Cost.Micros != 3000 {
		t.Errorf("Total = %+v cost %+v, want the failed attempt's call and the answering attempt's, once each", total, total.Cost)
	}
	events := w.bus.of(runID)
	if sum := tallied(t, events); !sameUsage(sum.Total, total) {
		t.Errorf("the call events add up to %+v, the run reports %+v", sum.Total, total)
	}
	if calls := usageOf(t, events); len(calls) != 2 || calls[0].AccountingID == calls[1].AccountingID {
		t.Errorf("usage.call events = %+v, want one per call with distinct keys", calls)
	}
	// Each attempt that executed the agent recorded its calls; the replay
	// executed nothing and recorded nothing.
	if keys := usageRecords(t, w, runID); fmt.Sprint(keys) != "[event:fiber.usage.a1#0 event:fiber.usage.a2#0]" {
		t.Errorf("usage records = %v, want the two attempts that called the model", keys)
	}
}

// A run that fails for good has no output, and what its attempts spent is
// still on its ledger records: every attempt's call, once.
func TestAFailedRunsUsageIsReadFromTheLedger(t *testing.T) {
	w := newWorld(t)
	exec := &spender{fail: 2, permanent: true}
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := rt.Result(ctx, runID); err == nil {
		t.Fatal("the run succeeded; the test needs it to fail")
	}
	_, records, err := w.ledger.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := runtime.RecordedUsage(records)
	if !ok {
		t.Fatalf("no usage recorded for a run that spent tokens; records %v", usageRecords(t, w, runID))
	}
	if got.Total.ModelCalls != 2 || got.Total.PromptTokens != 300 || got.Total.Cost.Micros != 3000 {
		t.Errorf("RecordedUsage = %+v, want both attempts' calls", got.Total)
	}
	if sum := tallied(t, w.bus.of(runID)); !sameUsage(sum.Total, got.Total) {
		t.Errorf("the call events add up to %+v, the ledger says %+v", sum.Total, got.Total)
	}
}

// spendingHeldModel says part of an answer, reports what the call has spent so
// far, and keeps the call open until it is cancelled: the model call a person
// stops.
type spendingHeldModel struct {
	started chan struct{}
}

func (spendingHeldModel) Capabilities() saigetypes.ModelCapabilities {
	return pricedModel{}.Capabilities()
}

func (h spendingHeldModel) Stream(ctx context.Context, _ saigetypes.Request) (<-chan saigetypes.Delta, error) {
	ch := make(chan saigetypes.Delta, 8)
	go func() {
		defer close(ch)
		for _, d := range saigetypes.PartDeltas(0, saigetypes.Text("The deploy failed because"))[:2] {
			ch <- d
		}
		ch <- saigetypes.UsageDelta{PromptTokens: 9_000, CompletionTokens: 12}
		close(h.started)
		<-ctx.Done()
	}()
	return ch, nil
}

// A stopped turn spent tokens on the call that was cut short. saige settles
// that call when it wraps the turn up, and the run records it: on the call's
// event, on lifecycle.stopped, and in the output.
func TestAStoppedRunRecordsTheUsageItSpent(t *testing.T) {
	w := newWorld(t)
	model := spendingHeldModel{started: make(chan struct{})}
	rt := w.runtime(t, sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model}))
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "why did the deploy fail?"))
	select {
	case <-model.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the model was never called")
	}
	w.stop(t, rt, "acme", runID, "Stopped by a user", "U1")
	out := w.result(t, rt, runID)

	if out.Stopped == nil {
		t.Fatalf("Output = %+v, want a stopped run", out)
	}
	if out.Usage == nil || out.Usage.Total.ModelCalls != 1 || out.Usage.Total.PromptTokens != 9_000 || out.Usage.Total.CompletionTokens != 12 {
		t.Fatalf("Output.Usage = %+v, want the call that was cut short", out.Usage)
	}
	if out.Usage.Total.Cost == nil || out.Usage.Total.Cost.Micros != 27_180 {
		t.Errorf("cost = %+v, want 9000 in at 3/Mtok and 12 out at 15/Mtok", out.Usage.Total.Cost)
	}
	events := w.bus.of(runID)
	body := stoppedBody(t, ended(t, events))
	if body.Usage == nil || !sameUsage(body.Usage.Total, out.Usage.Total) {
		t.Errorf("lifecycle.stopped usage = %+v, want the run's total", body.Usage)
	}
	if sum := tallied(t, events); !sameUsage(sum.Total, out.Usage.Total) {
		t.Errorf("the call events add up to %+v, the run reports %+v", sum.Total, out.Usage.Total)
	}
}

// chain sets up lead -> helper -> assistant: each may delegate to the next.
func chain(sp *specs) {
	for _, agent := range []string{"assistant", "helper", "lead"} {
		sp.revise(agent, agent+".")
	}
	sp.edit("helper", func(s *native.Spec) { s.Access.Spawn = []string{"assistant"} })
	sp.edit("lead", func(s *native.Spec) { s.Access.Spawn = []string{"helper"} })
}

// chainModels gives each agent a model that delegates to the next agent once
// and then answers. lead spends 100 then 150 tokens in, helper 50 then 60, and
// assistant, which delegates to nobody, 30.
func chainModels() map[string]saigetypes.Provider {
	return map[string]saigetypes.Provider{
		"lead": priced(
			spending(agenttest.ToolCallResponse("call-1", runtime.DelegateToolName("helper"), map[string]any{"text": "Check the tests"}), 100, 10),
			spending(agenttest.TextResponse("The lead is done."), 150, 15),
		),
		"helper": priced(
			spending(agenttest.ToolCallResponse("call-2", runtime.DelegateToolName("assistant"), map[string]any{"text": "Count them"}), 50, 5),
			spending(agenttest.TextResponse("The helper is done."), 60, 6),
		),
		"assistant": priced(spending(agenttest.TextResponse("Three fail."), 30, 3)),
	}
}

// A host that carries out delegations as runs of their own reports each child
// run's total to its parent. Three runs deep, each run's total is its own calls
// plus its children's totals, every call is in exactly one run's Own, and the
// origin's total is every call of the chain once, however often a child is
// reported.
func TestChildRunsRollUpThreeLevelsDeepWithoutDoubleCounting(t *testing.T) {
	w := newWorld(t)
	w.concurrency = 4
	chain(w.specs)

	var rt *runtime.Runtime
	delegate := func(ctx context.Context, parent runtime.Request, target, text string, _ func(event.Action) error) (string, error) {
		acc, err := accept(ctx, rt, tag(target, parent.RunID+"-"+target, text))
		if err != nil {
			return "", err
		}
		out, err := rt.Result(ctx, acc.RunID)
		if err != nil {
			return "", err
		}
		// Reported twice, as a host that retried its report would: the
		// parent counts the child once.
		for range 2 {
			if err := runtime.ReportChildUsage(ctx, acc.RunID); err != nil {
				return "", err
			}
		}
		return out.Text, nil
	}
	rt = w.runtime(t, sandboxOf(t, w.specs, chainModels(), runtime.WithDelegator(delegate)))
	w.work(t, rt, nil)

	lead := w.accept(t, rt, tag("lead", "evt-1", "How are the tests?"))
	leadOut := w.result(t, rt, lead)
	helper := runtime.RunKey("acme", lead+"-helper", "helper")
	helperOut := w.result(t, rt, helper)
	assistant := runtime.RunKey("acme", helper+"-assistant", "assistant")
	assistantOut := w.result(t, rt, assistant)

	tests := []struct {
		name                  string
		out                   runtime.Output
		own, children, totals [2]int // model calls, prompt tokens
	}{
		{"assistant", assistantOut, [2]int{1, 30}, [2]int{0, 0}, [2]int{1, 30}},
		{"helper", helperOut, [2]int{2, 110}, [2]int{1, 30}, [2]int{3, 140}},
		{"lead", leadOut, [2]int{2, 250}, [2]int{3, 140}, [2]int{5, 390}},
	}
	for _, tc := range tests {
		u := tc.out.Usage
		if u == nil {
			t.Fatalf("%s: Output.Usage is nil", tc.name)
		}
		check := func(part string, got event.Usage, want [2]int) {
			if got.ModelCalls != want[0] || got.PromptTokens != want[1] {
				t.Errorf("%s %s = %d calls, %d tokens in; want %d and %d", tc.name, part, got.ModelCalls, got.PromptTokens, want[0], want[1])
			}
		}
		check("own", u.Own, tc.own)
		check("children", u.Children, tc.children)
		check("total", u.Total, tc.totals)
	}

	// The origin's total costs exactly what every call of the chain cost.
	var micros int64
	calls := 0
	for _, runID := range []string{lead, helper, assistant} {
		for _, c := range usageOf(t, w.bus.of(runID)) {
			micros += c.Cost.Micros
			calls++
		}
	}
	if calls != 5 || leadOut.Usage.Total.Cost == nil || leadOut.Usage.Total.Cost.Micros != micros {
		t.Errorf("the chain made %d calls costing %d micros; the origin reports %+v", calls, micros, leadOut.Usage.Total.Cost)
	}
	children := 0
	for _, e := range w.bus.of(lead) {
		if e.Type == event.TypeUsageChild {
			children++
		}
	}
	if children != 2 {
		t.Errorf("the origin's stream carries %d usage.child reports, want the two the host made", children)
	}
}

// A sub-task delegated within a turn is not a run, so its calls are relayed
// onto the parent's stream one level deeper as they happen, and counted in the
// parent's Children: never in its Own, and never twice. Through the real
// sandbox, which allows one level of in-turn delegation.
func TestSubTaskCallsRollUpIntoTheRunThatDelegated(t *testing.T) {
	w := newWorld(t)
	chain(w.specs)
	models := chainModels()
	rt := w.runtime(t, sandboxOf(t, w.specs, models, runtime.WithReplicas(1)))
	w.work(t, rt, nil)

	lead := w.accept(t, rt, tag("lead", "evt-1", "How are the tests?"))
	out := w.result(t, rt, lead)
	events := w.bus.of(lead)
	calls := usageOf(t, events)

	depths := map[string][]int{}
	ids := map[string]bool{}
	for _, c := range calls {
		depths[c.Agent] = append(depths[c.Agent], c.Depth)
		if ids[c.AccountingID] {
			t.Errorf("AccountingID %q was published twice", c.AccountingID)
		}
		ids[c.AccountingID] = true
	}
	// A sub-task holds no spawn grant of its own, so the helper's attempt
	// to delegate further is refused by policy and the assistant never runs:
	// in-turn delegation is one level deep. The refused attempt still cost a
	// call, and it is counted. (The relay itself carries any depth; see
	// TestASubTaskOfASubTaskIsRelayedTwoLevelsDeepOnce.)
	if n := len(models["assistant"].(pricedModel).Calls); n != 0 {
		t.Fatalf("the assistant was called %d times; a sub-task's own delegation should be refused by policy", n)
	}
	if fmt.Sprint(depths["lead"]) != "[0 0]" || fmt.Sprint(depths["helper"]) != "[1 1]" {
		t.Fatalf("depths = %v, want lead's two calls at 0 and helper's two at 1", depths)
	}
	if out.Usage == nil {
		t.Fatal("Output.Usage is nil")
	}
	if sum := tallied(t, events); !sameUsage(sum.Total, out.Usage.Total) || !sameUsage(sum.Children, out.Usage.Children) {
		t.Errorf("the call events add up to %+v, the run reports %+v", sum, out.Usage)
	}
	if out.Usage.Own.ModelCalls != 2 || out.Usage.Own.PromptTokens != 250 {
		t.Errorf("Own = %+v, want the lead's two calls only", out.Usage.Own)
	}
	wantChildren := 0
	for _, d := range []string{"helper", "assistant"} {
		wantChildren += len(depths[d])
	}
	if out.Usage.Children.ModelCalls != wantChildren {
		t.Errorf("Children = %d calls, the sub-tasks made %d", out.Usage.Children.ModelCalls, wantChildren)
	}
}

// ReportChildUsage is the Delegator's, and only its: outside a delegation
// there is no parent to report to.
func TestReportingAChildRunOutsideADelegationIsRefused(t *testing.T) {
	err := runtime.ReportChildUsage(context.Background(), "r1")
	if !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("ReportChildUsage = %v, want ErrInvalid", err)
	}
}

// dropsTheAgentsResult is a ledger whose first write of the agent's successful
// result fails: the attempt answered, and durable never recorded it.
type dropsTheAgentsResult struct {
	durableLedger
	mu      sync.Mutex
	dropped bool
}

func (l *dropsTheAgentsResult) Record(ctx context.Context, rec drun.Record) (drun.Record, error) {
	if rec.Name == runtime.ActivityAgent && rec.Status == drun.RecordOK {
		l.mu.Lock()
		first := !l.dropped
		l.dropped = true
		l.mu.Unlock()
		if first {
			return drun.Record{}, errors.New("ledger unavailable")
		}
	}
	return l.durableLedger.Record(ctx, rec)
}

// An attempt that succeeded and whose result was never recorded is executed
// again, so it paid for a call the outcome it returned will never carry. That
// call is still counted: every attempt records its calls before it returns,
// success or not, and the run's total equals its call events.
func TestAnAttemptWhoseResultWasNeverRecordedIsStillCounted(t *testing.T) {
	w := newWorld(t)
	w.ledger = &dropsTheAgentsResult{durableLedger: w.ledger}
	exec := &spender{}
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	out := w.result(t, rt, runID)

	if n := exec.count(); n != 2 {
		t.Fatalf("the executor ran %d times, want 2: the first result was never recorded", n)
	}
	events := w.bus.of(runID)
	if calls := usageOf(t, events); len(calls) != 2 {
		t.Fatalf("published %d usage.call events, want 2", len(calls))
	}
	if out.Usage == nil || out.Usage.Total.ModelCalls != 2 || out.Usage.Total.PromptTokens != 300 {
		t.Fatalf("Output.Usage = %+v, want both attempts' calls", out.Usage)
	}
	if sum := tallied(t, events); !sameUsage(sum.Total, out.Usage.Total) {
		t.Errorf("the call events add up to %+v, the run reports %+v", sum.Total, out.Usage.Total)
	}
}

// A Delegator's report names a run and nothing else is taken from it. The
// runtime reads the child from the ledger and refuses a run that does not
// exist, another tenant's run, the parent itself and a run that has not ended,
// so a host cannot put a figure on a run that no child spent.
func TestAChildRunIsRolledUpOnlyWhenTheLedgerVouchesForIt(t *testing.T) {
	w := newWorld(t)
	w.concurrency = 4
	w.specs.revise("lead", "Lead.")
	w.specs.revise("helper", "Help.")
	w.specs.edit("lead", func(s *native.Spec) { s.Access.Spawn = []string{"helper"} })

	var rt *runtime.Runtime
	refusals := map[string]error{}
	var mu sync.Mutex
	delegate := func(ctx context.Context, parent runtime.Request, target, text string, _ func(event.Action) error) (string, error) {
		foreign := tag(target, "evt-foreign", "theirs")
		foreign.Tenant = "globex"
		facc, err := accept(ctx, rt, foreign)
		if err != nil {
			return "", err
		}
		if _, err := rt.Result(ctx, facc.RunID); err != nil {
			return "", err
		}
		pending := tag(target, "evt-pending", "later")
		pacc, err := accept(context.WithoutCancel(ctx), rt, pending)
		if err != nil {
			return "", err
		}
		mu.Lock()
		defer mu.Unlock()
		for name, id := range map[string]string{
			"unknown":        runtime.RunKey("acme", "no-such-tag", "helper"),
			"another tenant": facc.RunID,
			"the parent":     parent.RunID,
		} {
			refusals[name] = runtime.ReportChildUsage(ctx, id)
		}
		// Not ended, unless a worker raced it to the end: only a refusal
		// that says so counts.
		if err := runtime.ReportChildUsage(ctx, pacc.RunID); err != nil {
			refusals["not ended"] = err
		}
		return "nothing to roll up", nil
	}
	models := map[string]saigetypes.Provider{
		"lead": priced(
			spending(agenttest.ToolCallResponse("call-1", runtime.DelegateToolName("helper"), map[string]any{"text": "go"}), 100, 10),
			spending(agenttest.TextResponse("Done."), 100, 10),
		),
		"helper": priced(spending(agenttest.TextResponse("Helped."), 30, 3), spending(agenttest.TextResponse("Helped."), 30, 3)),
	}
	rt = w.runtime(t, sandboxOf(t, w.specs, models, runtime.WithDelegator(delegate)))
	w.work(t, rt, nil)

	lead := w.accept(t, rt, tag("lead", "evt-1", "go"))
	out := w.result(t, rt, lead)

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"unknown", "another tenant", "the parent"} {
		if !errors.Is(refusals[name], runtime.ErrNotAChild) {
			t.Errorf("reporting %s: %v, want ErrNotAChild", name, refusals[name])
		}
	}
	if err, ok := refusals["not ended"]; ok && !errors.Is(err, runtime.ErrNotAChild) {
		t.Errorf("reporting a run that has not ended: %v, want ErrNotAChild", err)
	}
	// Another tenant's run is refused in the same words as one that does
	// not exist, apart from the id.
	if a, b := fmt.Sprint(refusals["unknown"]), fmt.Sprint(refusals["another tenant"]); !strings.Contains(a, "no run") || !strings.Contains(b, "no run") {
		t.Errorf("refusals = %q and %q, want both to read as no such run", a, b)
	}
	if out.Usage == nil || out.Usage.Children.ModelCalls != 0 || out.Usage.Own.ModelCalls != 2 {
		t.Errorf("Output.Usage = %+v, want the lead's own calls and no children", out.Usage)
	}
	for _, e := range w.bus.of(lead) {
		if e.Type == event.TypeUsageChild {
			t.Fatalf("a refused report reached the parent's stream: %s", raw(e))
		}
	}
}
