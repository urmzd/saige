package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	drun "github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

// ── fixtures ────────────────────────────────────────────────────────

// countingTool is a tool that counts its calls, safely across goroutines.
type countingTool struct {
	def saigetypes.ToolDef
	// person makes it a tool that runs with the credential of the person
	// the turn acts for (native.PersonScoped).
	person bool

	mu   sync.Mutex
	args []map[string]any
}

func (c *countingTool) Definition() saigetypes.ToolDef { return c.def }

func (c *countingTool) ActsForPerson() bool { return c.person }

func (c *countingTool) Execute(_ context.Context, args map[string]any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args = append(c.args, args)
	return "deployed to " + fmt.Sprint(args["env"]), nil
}

func (c *countingTool) calls() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.args...)
}

func newDeployTool() *countingTool {
	return &countingTool{def: saigetypes.ToolDef{Name: "deploy", Description: "Deploy.", Parameters: saigetypes.ParameterSchema{
		Type: "object", Required: []string{"env"},
		Properties: map[string]saigetypes.PropertyDef{"env": {Type: "string", Enum: []string{"prod", "staging"}}},
	}}}
}

// parking is a world with an agent that can park: "ops" may call deploy only
// with approval, and may ask a person. Its model is one scripted provider
// every turn shares, so a continuation reads the script where the parked turn
// stopped.
type parking struct {
	*world
	deploy *countingTool
	model  *agenttest.ScriptedProvider
	exec   *runtime.Sandbox
}

func newParking(t *testing.T, w *world, script ...[]saigetypes.Delta) *parking {
	t.Helper()
	return newParkingWith(t, w, nil, script...)
}

// newParkingWith is newParking with the spec adjusted by mod.
func newParkingWith(t *testing.T, w *world, mod func(*native.Spec), script ...[]saigetypes.Delta) *parking {
	t.Helper()
	p := &parking{world: w, deploy: newDeployTool(), model: &agenttest.ScriptedProvider{Responses: script}}
	spec := native.Spec{
		Name: "ops", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "You run deploys.",
		Tools: []string{"deploy", native.AskPersonToolName}, Approvals: []string{"deploy"},
	}
	if mod != nil {
		mod(&spec)
	}
	w.specs.set(spec)
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{"ops": p.model, "docs-bot": bystander()},
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(p.deploy), nil
		}))
	return p
}

// bystander is a model for runs that only need to finish.
func bystander() *agenttest.ScriptedProvider {
	return &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.TextResponse("Documented."), agenttest.TextResponse("Documented."), agenttest.TextResponse("Documented."),
	}}
}

// runtime is the world's runtime over the parking executor, with the waker a
// deployment passes.
func (p *parking) rt(t *testing.T, opts ...runtime.Option) *runtime.Runtime {
	t.Helper()
	return p.runtime(t, p.exec, append([]runtime.Option{runtime.WithWaker(p.queue)}, opts...)...)
}

// workers runs a worker with a given concurrency and, when janitor is set,
// the janitor that re-enqueues parked runs: the backstop poll that applies an
// expired question's default.
func (w *world) workers(t *testing.T, rt *runtime.Runtime, concurrency int, janitor time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// A live turn must have time to renew its lease even with Postgres
	// and race instrumentation. Janitor frequency is independent of this.
	lease := 5 * time.Second
	go func() {
		defer close(done)
		_ = worker.New(w.ledger, w.queue,
			worker.WithLogger(slog.New(slog.DiscardHandler)),
			worker.WithRegistry(rt.Registry()),
			worker.WithConcurrency(concurrency),
			worker.WithJanitorEvery(janitor),
			worker.WithLeaseTTL(lease),
			worker.WithBackoff(func(int) time.Duration { return time.Millisecond }),
		).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// parkedOn waits until a run is parked and returns the question it published.
func (w *world) parkedOn(t *testing.T, rt *runtime.Runtime, runID string) event.Question {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if q, ok, err := rt.PendingQuestion(context.Background(), "acme", runID); err != nil {
			t.Fatalf("PendingQuestion: %v", err)
		} else if ok {
			for _, e := range w.bus.of(runID) {
				if e.Type != event.TypeParked {
					continue
				}
				body, err := decode[lifecycle](e)
				if err == nil && body.Question != nil && body.Question.ID == q.ID {
					return q
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never parked: %v", runID, kindsOf(w.bus.of(runID)))
	return event.Question{}
}

func (w *world) answer(t *testing.T, rt *runtime.Runtime, runID, qid string, a event.Answer) (event.Answer, bool) {
	t.Helper()
	got, first, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "acme", RunID: runID, QuestionID: qid, Answer: a, By: "ana"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	return got, first
}

func lifecycleOf(t *testing.T, events []event.Envelope, kind string) []lifecycle {
	t.Helper()
	var out []lifecycle
	for _, e := range events {
		if e.Type != kind {
			continue
		}
		body, err := decode[lifecycle](e)
		if err != nil {
			t.Fatalf("decode %s: %v", kind, err)
		}
		out = append(out, body)
	}
	return out
}

func call(id, name string, args map[string]any) []saigetypes.Delta {
	return agenttest.ToolCallResponse(id, name, args)
}

func sayThenCall(text, id, name string, args map[string]any) []saigetypes.Delta {
	out := saigetypes.PartDeltas(0, saigetypes.Text(text))
	return append(out,
		saigetypes.PartStart{Index: 1, Kind: saigetypes.KindToolCall, ID: id, Name: name},
		saigetypes.PartEnd{Index: 1, Part: saigetypes.ToolCallPart{ID: id, Name: name, Arguments: args}})
}

// ── tests ───────────────────────────────────────────────────────────

// Each kind of question parks the run: the question is published on
// lifecycle.parked and kept on the run, the worker is free for other runs
// while it waits, and the answer resumes the turn, which the model continues
// with the answer in front of it.
func TestEachQuestionKindParksPublishesItsQuestionReleasesItsWorkerAndResumesWithTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  []saigetypes.Delta
		kind   string
		answer event.Answer
		sees   string
	}{
		{"approval", sayThenCall("Deploying.", "c1", "deploy", map[string]any{"env": "prod"}), event.QuestionApproval,
			event.Answer{Decision: event.DecisionApprove}, "deployed to prod"},
		{"choice", call("c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}), event.QuestionChoice,
			event.Answer{Choice: "staging"}, "staging"},
		{"reply", call("c1", native.AskPersonToolName, map[string]any{"question": "Why deploy now?"}), event.QuestionReply,
			event.Answer{Text: "the fix is urgent"}, "the fix is urgent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParking(t, newWorld(t), tc.first, agenttest.TextResponse("Done."))
			rt := p.rt(t)
			p.workers(t, rt, 1, 0)

			runID := p.accept(t, rt, tag("ops", "evt-1", "ship the fix"))
			q := p.parkedOn(t, rt, runID)
			if q.Kind != tc.kind || q.Prompt == "" || q.ExpiresAt.IsZero() {
				t.Fatalf("question = %+v, want a %s with a prompt and an expiry", q, tc.kind)
			}
			if tc.kind == event.QuestionApproval && (q.Tool == nil || q.Tool.Name != "deploy" || !strings.Contains(string(q.Tool.Arguments), "prod")) {
				t.Fatalf("approval = %+v, want the deploy call and its arguments", q.Tool)
			}

			// The one worker slot is free: another run starts and finishes
			// while this one waits.
			other := p.accept(t, rt, tag("docs-bot", "evt-2", "explain"))
			if out := p.result(t, rt, other); out.Text == "" {
				t.Fatalf("the other run gave %+v", out)
			}
			if rec, err := p.ledger.GetRun(context.Background(), runID); err != nil || rec.Status != drun.StatusPending {
				t.Fatalf("the parked run is %v (%v), want pending", rec.Status, err)
			}

			got, first := p.answer(t, rt, runID, q.ID, tc.answer)
			if !first || got.By != "ana" || got.QuestionID != q.ID {
				t.Fatalf("Answer = %+v first=%v, want this answer, by ana", got, first)
			}
			out := p.result(t, rt, runID)
			if out.Stopped != nil || !strings.HasSuffix(out.Text, "Done.") {
				t.Fatalf("Output = %+v, want the turn finished", out)
			}
			resumed := lifecycleOf(t, p.bus.of(runID), event.TypeResumed)
			if len(resumed) == 0 || resumed[len(resumed)-1].Answer == nil || resumed[len(resumed)-1].Answer.QuestionID != q.ID {
				t.Fatalf("resumed events %+v, want the answer on one", resumed)
			}
			reqs := p.model.Requests()
			if len(reqs) != 2 {
				t.Fatalf("the model was called %d times, want 2: once before the question and once after", len(reqs))
			}
			if !strings.Contains(renderMessages(reqs[1].Messages), tc.sees) || !strings.Contains(renderMessages(reqs[1].Messages), "ship the fix") {
				t.Errorf("the resumed turn's model call does not carry the request and %q: %s", tc.sees, renderMessages(reqs[1].Messages))
			}
		})
	}
}

// The resumed turn has the whole conversation: the thread's earlier
// exchanges, the request, and what the turn said before it asked.
func TestTheResumedTurnSeesTheThreadTheRequestAndWhatItSaidBeforeAsking(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w,
		agenttest.TextResponse("Hello."),
		sayThenCall("Checking with you first.", "c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}),
		agenttest.TextResponse("Deploying to staging."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)

	firstRun := p.accept(t, rt, tag("ops", "evt-1", "hi"))
	p.result(t, rt, firstRun)
	runID := p.accept(t, rt, tag("ops", "evt-2", "ship it"))
	q := p.parkedOn(t, rt, runID)
	p.answer(t, rt, runID, q.ID, event.Answer{Choice: "staging"})
	out := p.result(t, rt, runID)

	if out.Text != "Checking with you first.Deploying to staging." {
		t.Errorf("answer = %q, want everything the run said across the park", out.Text)
	}
	sent := renderMessages(p.model.Requests()[2].Messages)
	for _, want := range []string{"hi", "Hello.", "ship it", "Checking with you first.", "staging"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the resumed call is missing %q: %s", want, sent)
		}
	}
	key := tag("ops", "evt-2", "ship it").key()
	got := said(journal(t, w.threads, key))
	if last := got[len(got)-1]; last != "assistant: Checking with you first.Deploying to staging." {
		t.Errorf("the thread remembers %q, want the whole answer", last)
	}
}

// A parked run holds nothing in the process that parked it: a restarted
// process answers it and finishes it.
func TestARunParkedAcrossARestartResumesOnTheAnswer(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w, call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := p.rt(t)
	ctx, stopWorker := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.New(w.ledger, w.queue, worker.WithRegistry(rt.Registry()), worker.WithJanitorEvery(0),
			worker.WithLogger(slog.New(slog.DiscardHandler))).Run(ctx)
	}()
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)
	stopWorker()
	<-done

	after := w.restarted()
	p2 := &parking{world: after, deploy: p.deploy, model: p.model, exec: p.exec}
	rt2 := p2.rt(t)
	after.workers(t, rt2, 1, 0)
	if got, ok, err := rt2.PendingQuestion(context.Background(), "acme", runID); err != nil || !ok || got.ID != q.ID {
		t.Fatalf("after the restart the pending question is %+v %v %v", got, ok, err)
	}
	after.answer(t, rt2, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	if out := after.result(t, rt2, runID); out.Text != "Done." {
		t.Fatalf("Output = %+v", out)
	}
	if n := len(p.deploy.calls()); n != 1 {
		t.Errorf("deploy ran %d times, want once", n)
	}
}

// Answers that race are recorded once: every caller is told the same answer,
// exactly one is told it was theirs, and the turn resumes once.
func TestConcurrentAnswersResumeTheRunExactlyOnce(t *testing.T) {
	p := newParking(t, newWorld(t), call("c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}),
		agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 4, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)

	const n = 12
	var wg sync.WaitGroup
	got := make([]event.Answer, n)
	firsts := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			choice := []string{"prod", "staging"}[i%2]
			got[i], firsts[i], errs[i] = rt.Answer(context.Background(), runtime.AnswerRequest{
				Tenant: "acme", RunID: runID, QuestionID: q.ID, Answer: event.Answer{Choice: choice}, By: fmt.Sprint("u", i)})
		}(i)
	}
	wg.Wait()
	winners := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("answer %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(got[i], got[0]) {
			t.Fatalf("answer %d was told %+v, answer 0 %+v", i, got[i], got[0])
		}
		if firsts[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d callers were told their answer was recorded, want 1", winners)
	}
	p.result(t, rt, runID)
	if calls := p.model.CallCount(); calls != 2 {
		t.Errorf("the model was called %d times, want 2", calls)
	}
	if n := len(lifecycleOf(t, p.bus.of(runID), event.TypeCompleted)); n != 1 {
		t.Errorf("the run completed %d times", n)
	}
}

// A second answer, whatever it says, is told the first.
func TestASecondAnswerReturnsTheFirstOutcome(t *testing.T) {
	p := newParking(t, newWorld(t), call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)

	first, mine := p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	second, mine2 := p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionDeny, Note: "changed my mind"})
	if !mine || mine2 || !reflect.DeepEqual(first, second) || second.Decision != event.DecisionApprove {
		t.Fatalf("first %+v (%v), second %+v (%v): want the second told the first", first, mine, second, mine2)
	}
	p.result(t, rt, runID)
	third, mine3 := p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionDeny})
	if mine3 || third.Decision != event.DecisionApprove {
		t.Fatalf("after the run ended an answer was told %+v (%v)", third, mine3)
	}
}

// An answer that does not fit its question is refused and records nothing,
// and so is one for a question that does not exist.
func TestAnAnswerThatDoesNotFitTheQuestionIsRefused(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w,
		call("c1", "deploy", map[string]any{"env": "prod"}),
		agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)

	for _, tc := range []struct {
		name string
		qid  string
		a    event.Answer
		want error
	}{
		{"no decision", q.ID, event.Answer{}, runtime.ErrInvalid},
		{"an unknown decision", q.ID, event.Answer{Decision: "maybe"}, runtime.ErrInvalid},
		{"a denial with arguments", q.ID, event.Answer{Decision: event.DecisionDeny, Arguments: json.RawMessage(`{"env":"staging"}`)}, runtime.ErrInvalid},
		{"arguments that are not an object", q.ID, event.Answer{Decision: event.DecisionApprove, Arguments: json.RawMessage(`["prod"]`)}, runtime.ErrInvalid},
		{"arguments outside the schema", q.ID, event.Answer{Decision: event.DecisionApprove, Arguments: json.RawMessage(`{"env":"moon"}`)}, runtime.ErrInvalid},
		{"arguments missing a required field", q.ID, event.Answer{Decision: event.DecisionApprove, Arguments: json.RawMessage(`{}`)}, runtime.ErrInvalid},
		{"an unknown question", "int_nope", event.Answer{Decision: event.DecisionApprove}, runtime.ErrNoQuestion},
	} {
		_, _, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "acme", RunID: runID, QuestionID: tc.qid, Answer: tc.a, By: "ana"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, ok, _ := rt.PendingQuestion(context.Background(), "acme", runID); !ok {
		t.Fatal("a refused answer resolved the question")
	}
	// A choice and a reply are held to their questions as well.
	for _, tc := range []struct {
		q event.Question
		a event.Answer
	}{
		{event.Question{ID: "x", Kind: event.QuestionChoice, Options: []string{"a"}}, event.Answer{Choice: "b"}},
		{event.Question{ID: "x", Kind: event.QuestionReply}, event.Answer{Text: "  "}},
	} {
		if err := runtime.CheckAnswer(tc.q, tc.a); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("%s answered %+v: err = %v, want ErrInvalid", tc.q.Kind, tc.a, err)
		}
	}

	edited, _ := p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove, Arguments: json.RawMessage(`{ "env": "staging" }`)})
	if string(edited.Arguments) != `{"env":"staging"}` {
		t.Errorf("recorded arguments %s", edited.Arguments)
	}
	p.result(t, rt, runID)
	if calls := p.deploy.calls(); len(calls) != 1 || calls[0]["env"] != "staging" {
		t.Errorf("deploy ran with %v, want the edited arguments once", calls)
	}
}

// Nobody answers: once the question is past its deadline the next poll of
// the run applies its default, a denial for an approval, and the run
// continues without the tool. A late answer is told the default.
func TestAnUnansweredQuestionTakesItsDefaultWhenItExpires(t *testing.T) {
	p := newParking(t, newWorld(t), call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Not deployed."))
	rt := p.rt(t, runtime.WithQuestionTTL(300*time.Millisecond))
	p.workers(t, rt, 2, 100*time.Millisecond)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)
	if q.Default == nil || q.Default.Decision != event.DecisionDeny {
		t.Fatalf("the approval's default is %+v, want a denial", q.Default)
	}

	out := p.result(t, rt, runID)
	if out.Text != "Not deployed." || len(p.deploy.calls()) != 0 {
		t.Fatalf("Output = %+v with %d deploys, want the run finished without the tool", out, len(p.deploy.calls()))
	}
	resumed := lifecycleOf(t, p.bus.of(runID), event.TypeResumed)
	if len(resumed) == 0 || resumed[len(resumed)-1].Answer == nil || !resumed[len(resumed)-1].Answer.Expired {
		t.Fatalf("resumed = %+v, want the expired default on it", resumed)
	}
	if !strings.Contains(renderMessages(p.model.Requests()[1].Messages), "nobody answered") {
		t.Errorf("the model was not told nobody answered: %s", renderMessages(p.model.Requests()[1].Messages))
	}
	late, mine := p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	if mine || !late.Expired || late.Decision != event.DecisionDeny {
		t.Errorf("a late answer was told %+v (%v), want the default", late, mine)
	}
}

// A question past its deadline that a person answers anyway resolves to its
// default too: the deadline is the question's, not the poll's.
func TestAnAnswerAfterTheDeadlineRecordsTheDefault(t *testing.T) {
	p := newParking(t, newWorld(t),
		call("c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}, "default": "staging"}),
		agenttest.TextResponse("Done."))
	rt := p.rt(t, runtime.WithQuestionTTL(50*time.Millisecond))
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)
	time.Sleep(80 * time.Millisecond)

	got, mine := p.answer(t, rt, runID, q.ID, event.Answer{Choice: "prod"})
	if mine || !got.Expired || got.Choice != "staging" {
		t.Fatalf("Answer after the deadline = %+v (%v), want the stated default", got, mine)
	}
	p.result(t, rt, runID)
	if !strings.Contains(renderMessages(p.model.Requests()[1].Messages), "staging") {
		t.Error("the model was not given the default")
	}
}

// Another tenant can neither answer a question nor learn that it exists:
// every refusal reads like a question that is not there.
func TestAnotherTenantCannotAnswerOrSeeAQuestion(t *testing.T) {
	p := newParking(t, newWorld(t), call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)

	_, _, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "globex", RunID: runID, QuestionID: q.ID,
		Answer: event.Answer{Decision: event.DecisionApprove}, By: "mallory"})
	if !errors.Is(err, runtime.ErrNoQuestion) {
		t.Fatalf("another tenant's answer: %v, want ErrNoQuestion", err)
	}
	_, _, unknown := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "globex", RunID: "rnope", QuestionID: q.ID,
		Answer: event.Answer{Decision: event.DecisionApprove}})
	if unknown.Error() != err.Error() {
		t.Errorf("another tenant's run reads %q, an unknown run %q", err, unknown)
	}
	if _, ok, err := rt.PendingQuestion(context.Background(), "globex", runID); ok || err != nil {
		t.Errorf("another tenant sees the question: %v %v", ok, err)
	}
	if _, ok, _ := rt.PendingQuestion(context.Background(), "acme", runID); !ok {
		t.Error("the question was resolved by another tenant")
	}
}

// Stop ends a parked run without resuming its turn: no model call, no tool,
// the run ends stopped with what it said before it asked, and the question is
// closed to answers.
func TestStoppingAParkedRunEndsItWithoutResumingItsTurn(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w, sayThenCall("Deploying.", "c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("never"))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	tg := tag("ops", "evt-1", "ship")
	runID := p.accept(t, rt, tg)
	q := p.parkedOn(t, rt, runID)

	p.stop(t, rt, "acme", runID, "Not today", "U1")
	out := p.result(t, rt, runID)
	if out.Stopped == nil || out.Stopped.Reason != "Not today" || out.Stopped.By != "U1" || out.Text != "Deploying." {
		t.Fatalf("Output = %+v, want stopped with the text before the question", out)
	}
	if calls := p.model.CallCount(); calls != 1 || len(p.deploy.calls()) != 0 {
		t.Fatalf("after the stop the model was called %d times and deploy ran %d times", calls, len(p.deploy.calls()))
	}
	ended(t, p.bus.of(runID))
	if _, _, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "acme", RunID: runID, QuestionID: q.ID,
		Answer: event.Answer{Decision: event.DecisionApprove}}); !errors.Is(err, runtime.ErrQuestionClosed) {
		t.Errorf("answering a stopped run's question: %v, want ErrQuestionClosed", err)
	}
	want := "assistant: Deploying.\n\n" + runtime.StopMarker
	if got := said(journal(t, w.threads, tg.key())); got[len(got)-1] != want {
		t.Errorf("the thread remembers %q, want %q", got[len(got)-1], want)
	}
}

// Usage spent before the park is counted once, with what the resumed turn
// spent after it, and the total is the sum of the call events.
func TestUsageBeforeAParkIsCountedOnce(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w)
	model := priced(
		spending(call("c1", "deploy", map[string]any{"env": "prod"}), 100, 10),
		spending(agenttest.TextResponse("Done."), 200, 20))
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{"ops": model},
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(p.deploy), nil
		}))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	q := p.parkedOn(t, rt, runID)
	p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	out := p.result(t, rt, runID)

	if out.Usage == nil {
		t.Fatal("no usage on the output")
	}
	if got := out.Usage.Own; got.ModelCalls != 2 || got.PromptTokens != 300 || got.CompletionTokens != 30 {
		t.Errorf("own usage = %+v, want both calls once: 2 calls, 300 in, 30 out", got)
	}
	if fromEvents := tallied(t, p.bus.of(runID)); !sameUsage(fromEvents.Total, out.Usage.Total) {
		t.Errorf("events tally %+v, output %+v", fromEvents.Total, out.Usage.Total)
	}
}

// A parked-then-resumed run replayed from its records makes the same
// decisions: the journal is offered the same bytes, and nothing is asked,
// called or run twice.
func TestReplayingAParkedThenResumedRunIsDeterministic(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w, sayThenCall("Deploying.", "c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	var once sync.Once
	w.bus.rec.failWhen(func(e event.Envelope) error {
		var err error
		if e.Type == event.TypeCompleted {
			once.Do(func() { err = errors.New("broker outage") })
		}
		return err
	})
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	tg := tag("ops", "evt-1", "ship")
	runID := p.accept(t, rt, tg)
	q := p.parkedOn(t, rt, runID)
	p.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	out := p.result(t, rt, runID)

	if out.Text != "Deploying.Done." {
		t.Fatalf("Output = %+v", out)
	}
	if p.model.CallCount() != 2 || len(p.deploy.calls()) != 1 {
		t.Fatalf("model %d calls, deploy %d: a replay asked or ran something again", p.model.CallCount(), len(p.deploy.calls()))
	}
	offered := w.threads.offered()
	if len(offered) != 2 {
		t.Fatalf("the journal was offered the exchange %d times, want once per attempt that finished the turn", len(offered))
	}
	if !reflect.DeepEqual(offered[0], offered[1]) {
		t.Errorf("a replay offered the journal different bytes:\n %q\n %q", offered[0], offered[1])
	}
	if parked := lifecycleOf(t, p.bus.of(runID), event.TypeParked); len(parked) != 1 {
		t.Errorf("the question was announced %d times, want once", len(parked))
	} else if parked[0].Question == nil || parked[0].Question.ID != q.ID || parked[0].Questions != nil || parked[0].Reason != "waiting on a person" {
		t.Errorf("a single question was announced as %+v, want it alone, as before batches", parked[0])
	}
	if resumed := lifecycleOf(t, p.bus.of(runID), event.TypeResumed); len(resumed) == 0 || resumed[0].Answer == nil || resumed[0].Answers != nil {
		t.Errorf("a single answer resumed the run as %+v, want it alone", resumed)
	}
	_, records, err := w.ledger.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	questions := 0
	for _, rec := range records {
		if strings.HasPrefix(rec.Name, "fiber.question.") {
			questions++
		}
	}
	if questions != 1 {
		t.Errorf("the ledger holds %d question records, want 1", questions)
	}
}

// A parked run polled by the janitor with nothing new says nothing: waiting
// is silent on the bus.
func TestAParkedRunPolledWithNoAnswerPublishesNothing(t *testing.T) {
	p := newParking(t, newWorld(t), call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 50*time.Millisecond)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	p.parkedOn(t, rt, runID)
	before := len(p.bus.of(runID))
	// The janitor only sweeps runs older than one lease. Include several
	// eligible polls, rather than only polling while the run is too young.
	time.Sleep(5*time.Second + 400*time.Millisecond)
	if after := len(p.bus.of(runID)); after != before {
		t.Errorf("polls published %d events: %v", after-before, kindsOf(p.bus.of(runID))[before:])
	}
}

// A delegated child run can park like any run. Its parent waits in its
// Delegator, and the host shows the parent waiting on the child's question.
func TestAChildRunParksAndItsParentShowsItsQuestion(t *testing.T) {
	w := newWorld(t)
	w.concurrency = 4
	p := newParking(t, w,
		call("c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}),
		agenttest.TextResponse("Child done."))
	w.specs.set(native.Spec{
		Name: "lead", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "Lead.",
		Access: native.Access{Spawn: []string{"ops"}},
	})
	leadModel := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		call("d1", runtime.DelegateToolName("ops"), map[string]any{"text": "deploy"}),
		agenttest.TextResponse("Lead done."),
	}}

	var rt *runtime.Runtime
	child := make(chan string, 1)
	delegate := func(ctx context.Context, parent runtime.Request, target, text string, _ func(event.Action) error) (string, error) {
		acc, err := accept(ctx, rt, fixture{ID: parent.RunID + "/child", Tenant: parent.Tenant, Agent: target, Origin: "delegation", Text: text, At: time.Now()})
		if err != nil {
			return "", err
		}
		child <- acc.RunID
		if err := runtime.WatchChildQuestions(ctx, acc.RunID); err != nil {
			return "", err
		}
		out, err := rt.Result(ctx, acc.RunID)
		if err != nil {
			return "", err
		}
		return out.Text, nil
	}
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{"ops": p.model, "lead": leadModel},
		runtime.WithDelegator(delegate),
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(p.deploy), nil
		}))
	rt = p.rt(t)
	p.workers(t, rt, 4, 0)

	parentID := p.accept(t, rt, tag("lead", "evt-1", "ship"))
	childID := <-child
	q := p.parkedOn(t, rt, childID)

	// The parent's stream says its delegation waits on the child's question.
	deadline := time.Now().Add(10 * time.Second)
	var waiting *event.ChildParked
	for waiting == nil && time.Now().Before(deadline) {
		for _, e := range p.bus.of(parentID) {
			if e.Type == event.TypeChildParked {
				body, err := decode[event.ChildParked](e)
				if err == nil {
					waiting = &body
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if waiting == nil || waiting.RunID != childID || waiting.Question.ID != q.ID || waiting.Agent != "ops" {
		t.Fatalf("the parent's stream shows %+v, want the child's question", waiting)
	}

	p.answer(t, rt, childID, q.ID, event.Answer{Choice: "prod"})
	if out := p.result(t, rt, parentID); out.Text != "Lead done." {
		t.Fatalf("parent Output = %+v", out)
	}
	if out := p.result(t, rt, childID); out.Text != "Child done." {
		t.Fatalf("child Output = %+v", out)
	}
}

func renderMessages(msgs []saigetypes.Message) string {
	b, _ := json.Marshal(msgs)
	return string(b)
}

// requested is a tag raised by ana through GitHub.
func requested(id string) fixture {
	tg := tag("ops", id, "ship")
	tg.Actor = event.Actor{ID: "ana"}
	return tg
}

func (p *parking) answerAs(rt *runtime.Runtime, runID, qid, by, via string, a event.Answer) error {
	_, _, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "acme", RunID: runID, QuestionID: qid, Answer: a, By: by, Via: via})
	return err
}

// An approval is a second pair of eyes. The person whose request raised the
// call cannot approve it under any name they go by, and neither can the
// credential that raised the run, whoever it names; a surface trusted to act
// for others may name someone else, who is checked like anyone.
func TestTheRequesterCannotApproveTheirOwnCallByDefault(t *testing.T) {
	p := newParking(t, newWorld(t), call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, requested("evt-1"))
	q := p.parkedOn(t, rt, runID)
	approve := event.Answer{Decision: event.DecisionApprove}

	for _, tc := range []struct{ name, by, via string }{
		{"the requester", "ana", "ana"},
		{"the requester by their qualified name", "github:ana", "slack-surface"},
		{"the requester's own credential naming someone else", "bob", "ana"},
	} {
		if err := p.answerAs(rt, runID, q.ID, tc.by, tc.via, approve); !errors.Is(err, runtime.ErrNotAllowed) {
			t.Errorf("%s: err = %v, want ErrNotAllowed", tc.name, err)
		}
	}
	if _, ok, _ := rt.PendingQuestion(context.Background(), "acme", runID); !ok {
		t.Fatal("a refused approval resolved the question")
	}
	if err := p.answerAs(rt, runID, q.ID, "bob", "slack-surface", approve); err != nil {
		t.Fatalf("another person through a trusted surface: %v", err)
	}
	p.result(t, rt, runID)
	if n := len(p.deploy.calls()); n != 1 {
		t.Errorf("deploy ran %d times", n)
	}
}

// A call that runs with the credential of the person the run acts for is
// theirs to change. Another approver may approve it as asked, but not with
// edited arguments, which would act as that person on something they did
// not ask for; the person may, under the name the host knows them by.
func TestOnlyThePersonACallActsAsMayEditIt(t *testing.T) {
	approve := event.Answer{Decision: event.DecisionApprove}
	staging := event.Answer{Decision: event.DecisionApprove, Arguments: json.RawMessage(`{"env":"staging"}`)}
	for _, tc := range []struct {
		name   string
		actor  event.Actor
		by     string
		answer event.Answer
		env    string
	}{
		{"another approver, as asked", event.Actor{ID: "ana"}, "bob", approve, "prod"},
		{"the requester, edited", event.Actor{ID: "ana"}, "ana", staging, "staging"},
		{"the person a surface's run acts for, edited", event.Actor{ID: "U0ANA", User: "ana"}, "ana", staging, "staging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParkingWith(t, newWorld(t), func(s *native.Spec) { s.SelfApproval = true },
				call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
			p.deploy.person = true
			rt := p.rt(t)
			p.workers(t, rt, 2, 0)
			tg := tag("ops", "evt-1", "ship")
			tg.Actor = tc.actor
			runID := p.accept(t, rt, tg)
			q := p.parkedOn(t, rt, runID)
			if q.Tool == nil || !q.Tool.ActsForPerson {
				t.Fatalf("the question does not say the call acts as the person: %+v", q.Tool)
			}
			for _, by := range []string{"bob", "carol", ""} {
				if err := p.answerAs(rt, runID, q.ID, by, by, staging); !errors.Is(err, runtime.ErrNotAllowed) {
					t.Errorf("%q edited the call: err = %v, want ErrNotAllowed", by, err)
				}
			}
			if _, ok, _ := rt.PendingQuestion(context.Background(), "acme", runID); !ok {
				t.Fatal("a refused edit resolved the question")
			}
			if err := p.answerAs(rt, runID, q.ID, tc.by, tc.by, tc.answer); err != nil {
				t.Fatalf("%s: %v", tc.by, err)
			}
			p.result(t, rt, runID)
			if calls := p.deploy.calls(); len(calls) != 1 || calls[0]["env"] != tc.env {
				t.Errorf("deploy ran with %v, want env %s once", calls, tc.env)
			}
		})
	}
}

// A spec that opts into self-approval lets the requester approve, and keeps
// letting anyone else.
func TestSelfApprovalIsAllowedWhenTheSpecOptsIn(t *testing.T) {
	p := newParkingWith(t, newWorld(t), func(s *native.Spec) { s.SelfApproval = true },
		call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, requested("evt-1"))
	q := p.parkedOn(t, rt, runID)
	if err := p.answerAs(rt, runID, q.ID, "ana", "ana", event.Answer{Decision: event.DecisionApprove}); err != nil {
		t.Fatalf("self-approval with the opt-out: %v", err)
	}
	p.result(t, rt, runID)
}

// Approvers and answerers, when a spec names them, are the only people who
// may answer: by name, or by a role the host resolves. Without a resolver a
// role admits nobody.
func TestOnlyTheSpecsApproversAndAnswerersMayAnswer(t *testing.T) {
	roles := runtime.WithRoles(func(_ context.Context, tenant, who string) ([]string, error) {
		if tenant == "acme" && who == "dave" {
			return []string{"oncall"}, nil
		}
		return nil, nil
	})
	for _, tc := range []struct {
		name    string
		by      string
		roles   bool
		allowed bool
	}{
		{"someone not named", "bob", true, false},
		{"a named approver", "carol", true, true},
		{"a holder of a named role", "dave", true, true},
		{"a role with no resolver", "dave", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParkingWith(t, newWorld(t), func(s *native.Spec) { s.Approvers = []string{"carol", "role:oncall"} },
				call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
			var opts []runtime.Option
			if tc.roles {
				opts = append(opts, roles)
			}
			rt := p.rt(t, opts...)
			p.workers(t, rt, 2, 0)
			runID := p.accept(t, rt, requested("evt-1"))
			q := p.parkedOn(t, rt, runID)
			err := p.answerAs(rt, runID, q.ID, tc.by, "surface", event.Answer{Decision: event.DecisionApprove})
			if tc.allowed != (err == nil) || (err != nil && !errors.Is(err, runtime.ErrNotAllowed)) {
				t.Fatalf("err = %v, want allowed=%v", err, tc.allowed)
			}
		})
	}

	t.Run("choices keep tenant-wide answering unless restricted", func(t *testing.T) {
		ask := call("c1", native.AskPersonToolName, map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}})
		open := newParking(t, newWorld(t), ask, agenttest.TextResponse("Done."))
		rt := open.rt(t)
		open.workers(t, rt, 2, 0)
		runID := open.accept(t, rt, requested("evt-1"))
		q := open.parkedOn(t, rt, runID)
		if err := open.answerAs(rt, runID, q.ID, "ana", "ana", event.Answer{Choice: "prod"}); err != nil {
			t.Fatalf("the requester answering their agent's question: %v", err)
		}

		closed := newParkingWith(t, newWorld(t), func(s *native.Spec) { s.Answerers = []string{"carol"} }, ask, agenttest.TextResponse("Done."))
		rt2 := closed.rt(t)
		closed.workers(t, rt2, 2, 0)
		runID = closed.accept(t, rt2, requested("evt-1"))
		q = closed.parkedOn(t, rt2, runID)
		if err := closed.answerAs(rt2, runID, q.ID, "bob", "bob", event.Answer{Choice: "prod"}); !errors.Is(err, runtime.ErrNotAllowed) {
			t.Fatalf("bob answering a question kept for carol: %v", err)
		}
		if err := closed.answerAs(rt2, runID, q.ID, "carol", "carol", event.Answer{Choice: "prod"}); err != nil {
			t.Fatalf("carol: %v", err)
		}
	})
}

// routed is a model that answers from the conversation it is sent, so any
// number of runs can share it in any order: the first call of a turn calls
// the tool, and a call that sees a tool result answers with it.
type routed struct {
	tool string
	args map[string]any
	done string
}

func (m routed) Stream(_ context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	var deltas []saigetypes.Delta
	result, answered := "", false
	for _, msg := range req.Messages {
		for _, p := range saigetypes.PartsOf(msg) {
			if tr, ok := p.(saigetypes.ToolResultPart); ok {
				result, answered = tr.Text(), true
			}
		}
	}
	if !answered {
		deltas = call("c1", m.tool, m.args)
	} else {
		deltas = agenttest.TextResponse(m.done + " " + result)
	}
	ch := make(chan saigetypes.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// delegating is a world where "lead" delegates to "ops", which asks a person
// which env, with delegations as child runs.
func childRunWorld(t *testing.T, w *world, opts ...runtime.Option) (*runtime.Runtime, *parking) {
	t.Helper()
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
	return p.rt(t, append([]runtime.Option{runtime.WithChildRuns()}, opts...)...), p
}

// pendingOf waits for a run to be parked on a question of a kind.
func pendingOf(t *testing.T, rt *runtime.Runtime, runID, kind string) event.Question {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if q, ok, err := rt.PendingQuestion(context.Background(), "acme", runID); err == nil && ok && q.Kind == kind {
			return q
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never parked on a %s", runID, kind)
	return event.Question{}
}

// A parent waiting on a delegated child parks instead of holding its worker,
// so as many parents as there are workers each delegating to a child that
// waits on a person all finish: the children run, are answered, and wake
// their parents. With parents that held their workers, no child could start.
func TestDelegatingParentsParkSoTheirChildrenRunAtAnyConcurrency(t *testing.T) {
	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprint(workers, " workers"), func(t *testing.T) {
			w := newWorld(t)
			rt, p := childRunWorld(t, w)
			p.workers(t, rt, workers, 0)

			var parents []string
			for i := range workers {
				tg := tag("lead", fmt.Sprint("evt-", i), "ship")
				tg.Actor = event.Actor{ID: "ana"}
				parents = append(parents, p.accept(t, rt, tg))
			}
			for _, parent := range parents {
				wait := pendingOf(t, rt, parent, event.QuestionDelegation)
				if wait.ChildRunID == "" || wait.Tool == nil || wait.Tool.Name != "tag_ops" {
					t.Fatalf("the parent waits on %+v, want a child run of ops", wait)
				}
				if _, _, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "acme", RunID: parent, QuestionID: wait.ID,
					Answer: event.Answer{Text: "done"}, By: "bob"}); !errors.Is(err, runtime.ErrInvalid) {
					t.Errorf("a person answered a delegation: %v", err)
				}
				q := pendingOf(t, rt, wait.ChildRunID, event.QuestionChoice)
				p.answer(t, rt, wait.ChildRunID, q.ID, event.Answer{Choice: "staging"})
			}
			for _, parent := range parents {
				out := p.result(t, rt, parent)
				if !strings.Contains(out.Text, "Child chose staging") {
					t.Errorf("parent answered %q, want the child's answer", out.Text)
				}
				if out.Usage == nil || len(lifecycleOf(t, p.bus.of(parent), event.TypeResumed)) == 0 {
					t.Errorf("parent %s: usage %v, resumed events %d", parent, out.Usage, len(lifecycleOf(t, p.bus.of(parent), event.TypeResumed)))
				}
				var children int
				for _, e := range p.bus.of(parent) {
					if e.Type == event.TypeUsageChild {
						children++
					}
				}
				if children != 1 {
					t.Errorf("parent %s rolled up %d child runs, want 1", parent, children)
				}
			}
		})
	}
}

// holdWaker wakes runs as the queue it wraps does, and then keeps the first
// caller that wakes run where it is until release is closed. A child run
// wakes its parent from inside its own body, so holding that call holds the
// child a step short of ended while the parent goes on.
type holdWaker struct {
	runtime.Waker
	run     atomic.Value // string
	once    sync.Once
	release chan struct{}
	done    sync.Once
}

// free lets the held caller go. It is safe to call more than once.
func (h *holdWaker) free() { h.done.Do(func() { close(h.release) }) }

func (h *holdWaker) Enqueue(ctx context.Context, runID string, delay time.Duration) error {
	err := h.Waker.Enqueue(ctx, runID, delay)
	if run, _ := h.run.Load().(string); run != "" && run == runID {
		h.once.Do(func() { <-h.release })
	}
	return err
}

// A parent rolls up a child that has told it how it ended, even when the
// parent resumes before the ledger records the child as ended. The child
// answers its parent as the last thing its body does, so there is always a
// moment in which the parent can run ahead; here the child is held in that
// moment until the parent has finished. A parent that looked only at the
// child's status found it still running and dropped what it spent.
func TestAParentRollsUpAChildThatAnsweredBeforeTheLedgerRecordsItsEnd(t *testing.T) {
	w := newWorld(t)
	hold := &holdWaker{Waker: w.queue, release: make(chan struct{})}
	rt, p := childRunWorld(t, w, runtime.WithWaker(hold))
	p.workers(t, rt, 2, 0)
	// Registered after the workers, so it runs before they are stopped: a
	// failed test must not leave a worker held.
	t.Cleanup(hold.free)

	parent := p.accept(t, rt, tag("lead", "evt-1", "ship"))
	wait := pendingOf(t, rt, parent, event.QuestionDelegation)
	q := pendingOf(t, rt, wait.ChildRunID, event.QuestionChoice)
	// From here the first wake of the parent is the child's, and it is held.
	hold.run.Store(parent)
	p.answer(t, rt, wait.ChildRunID, q.ID, event.Answer{Choice: "staging"})

	out := p.result(t, rt, parent)
	if !strings.Contains(out.Text, "Child chose staging") {
		t.Fatalf("parent answered %q, want the child's answer", out.Text)
	}
	// The parent is done and the child is still inside its body.
	if rec, _, err := p.ledger.Load(context.Background(), wait.ChildRunID); err != nil || rec.Status.Terminal() {
		t.Fatalf("the child is %v (%v) when its parent ends, want it not yet ended", rec.Status, err)
	}
	var rolled []event.UsageChild
	for _, e := range p.bus.of(parent) {
		if e.Type == event.TypeUsageChild {
			c, err := decode[event.UsageChild](e)
			if err != nil {
				t.Fatal(err)
			}
			rolled = append(rolled, c)
		}
	}
	if len(rolled) != 1 || rolled[0].RunID != wait.ChildRunID {
		t.Fatalf("the parent rolled up %+v, want its one child %s", rolled, wait.ChildRunID)
	}

	hold.free()
	child := p.result(t, rt, wait.ChildRunID)
	if child.Usage == nil {
		t.Fatalf("the child's output holds no usage: %+v", child)
	}
	got, _ := json.Marshal(rolled[0].Usage)
	want, _ := json.Marshal(child.Usage.Total)
	if string(got) != string(want) {
		t.Errorf("the parent rolled up %+v, want what the child's output holds, %+v", rolled[0].Usage, child.Usage)
	}
}

// Stopping a parent that waits on its child stops the child too, with the
// same words, and neither resumes.
func TestStoppingAParentWaitingOnAChildStopsBoth(t *testing.T) {
	w := newWorld(t)
	rt, p := childRunWorld(t, w)
	p.workers(t, rt, 2, 0)
	parent := p.accept(t, rt, tag("lead", "evt-1", "ship"))
	wait := pendingOf(t, rt, parent, event.QuestionDelegation)
	pendingOf(t, rt, wait.ChildRunID, event.QuestionChoice)

	p.stop(t, rt, "acme", parent, "Not today", "U1")
	if out := p.result(t, rt, parent); out.Stopped == nil || out.Stopped.Reason != "Not today" {
		t.Fatalf("parent Output = %+v", out)
	}
	if out := p.result(t, rt, wait.ChildRunID); out.Stopped == nil || out.Stopped.Reason != "Not today" {
		t.Fatalf("child Output = %+v", out)
	}
}

// A run that parks again and again stores its conversation once: each park
// records what the turn added since the last one, and the continuation is
// still given the whole of it.
func TestRepeatedParksStoreTheTranscriptOnce(t *testing.T) {
	w := newWorld(t)
	p := newParking(t, w,
		call("c1", native.AskPersonToolName, map[string]any{"question": "First?"}),
		call("c2", native.AskPersonToolName, map[string]any{"question": "Second?"}),
		call("c3", native.AskPersonToolName, map[string]any{"question": "Third?"}),
		agenttest.TextResponse("Done."))
	rt := p.rt(t)
	p.workers(t, rt, 2, 0)
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))
	for _, reply := range []string{"one", "two", "three"} {
		q := pendingOf(t, rt, runID, event.QuestionReply)
		p.answer(t, rt, runID, q.ID, event.Answer{Text: reply})
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if next, ok, _ := rt.PendingQuestion(context.Background(), "acme", runID); !ok || next.ID != q.ID {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	p.result(t, rt, runID)
	last := renderMessages(p.model.Requests()[3].Messages)
	for _, want := range []string{"one", "two", "three", "ship"} {
		if !strings.Contains(last, want) {
			t.Errorf("the last call is missing %q: %s", want, last)
		}
	}
	_, records, err := w.ledger.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, rec := range records {
		if rec.Name != runtime.ActivityAgent && rec.Name != runtime.ActivityResume {
			continue
		}
		var out runtime.Outcome
		if err := json.Unmarshal(rec.Output, &out); err != nil {
			t.Fatal(err)
		}
		if out.Parked != nil {
			stored += len(out.Parked.Transcript)
		}
	}
	// Three parks; the conversation they share is stored once: the first
	// park holds nothing before its message, and each later one holds only
	// the previous call and its result.
	if stored != 4 {
		t.Errorf("the parks store %d transcript messages, want 4", stored)
	}
}

// ── holds ───────────────────────────────────────────────────────────

// holdTool holds its own run when it is called, as a host that notices
// something mid-turn (a run that reached its limit) would.
type holdTool struct {
	rt    func() *runtime.Runtime
	hold  turn.Hold
	runID string
}

func (h *holdTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "spend", Description: "Spend.", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}

func (h *holdTool) Execute(context.Context, map[string]any) (string, error) {
	if err := h.rt().Hold(context.Background(), runtime.HoldRequest{Tenant: "acme", RunID: h.runID, Hold: h.hold}); err != nil {
		return "", err
	}
	return "spent", nil
}

// holding is a world whose "ops" agent deploys without approvals, and may
// call spend, which holds its own run.
type holding struct {
	*parking
	spend *holdTool
	live  *runtime.Runtime
}

func newHolding(t *testing.T, w *world, script ...[]saigetypes.Delta) *holding {
	t.Helper()
	h := &holding{parking: &parking{world: w, deploy: newDeployTool(), model: &agenttest.ScriptedProvider{Responses: script}}}
	h.spend = &holdTool{rt: func() *runtime.Runtime { return h.live }, hold: budgetHold("h1")}
	w.specs.set(native.Spec{
		Name: "ops", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "You run deploys.",
		Tools: []string{"deploy", "spend"},
	})
	h.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{"ops": h.model, "docs-bot": bystander()},
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(h.deploy, h.spend), nil
		}))
	return h
}

func (h *holding) rt(t *testing.T, opts ...runtime.Option) *runtime.Runtime {
	t.Helper()
	h.live = h.parking.rt(t, opts...)
	return h.live
}

func budgetHold(id string) turn.Hold {
	return turn.Hold{ID: id, Topic: "budget", Prompt: "ops reached its spend limit. Grant $5 more?", Data: json.RawMessage(`{"grant":5000000}`)}
}

func hold(t *testing.T, rt *runtime.Runtime, runID string, h turn.Hold) {
	t.Helper()
	if err := rt.Hold(context.Background(), runtime.HoldRequest{Tenant: "acme", RunID: runID, Hold: h}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
}

// A hold recorded before the run starts parks it at its first batch of tool
// calls, before any of them runs, on a question of kind hold carrying the
// host's hold. An approval runs the batch and the turn carries on, and the
// hold is not asked again at the next batch.
func TestAHeldRunParksAtItsNextToolCallsAndAnApprovalRunsThem(t *testing.T) {
	h := newHolding(t, newWorld(t),
		sayThenCall("Deploying.", "c1", "deploy", map[string]any{"env": "prod"}),
		call("c2", "deploy", map[string]any{"env": "staging"}),
		agenttest.TextResponse("Done."))
	rt := h.rt(t)
	runID := h.accept(t, rt, tag("ops", "evt-1", "ship"))
	hold(t, rt, runID, budgetHold("h1"))
	hold(t, rt, runID, budgetHold("h1")) // the same hold twice is one hold
	h.workers(t, rt, 1, 0)

	q := h.parkedOn(t, rt, runID)
	if q.Kind != event.QuestionHold || q.Hold == nil || q.Hold.ID != "h1" || q.Hold.Topic != "budget" ||
		string(q.Hold.Data) != `{"grant":5000000}` || !strings.Contains(q.Prompt, "spend limit") {
		t.Fatalf("question = %+v (hold %+v), want the host's hold", q, q.Hold)
	}
	if q.Default == nil || q.Default.Decision != event.DecisionDeny {
		t.Errorf("a hold's default is %+v, want a denial", q.Default)
	}
	if n := len(h.deploy.calls()); n != 0 {
		t.Fatalf("deploy ran %d times while the run was held", n)
	}

	got, first := h.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	if !first || got.Decision != event.DecisionApprove {
		t.Fatalf("Answer = %+v (%v)", got, first)
	}
	out := h.result(t, rt, runID)
	if out.Stopped != nil || out.Text != "Deploying.Done." {
		t.Fatalf("Output = %+v, want the turn finished", out)
	}
	if n := len(h.deploy.calls()); n != 2 {
		t.Errorf("deploy ran %d times, want both batches once each", n)
	}
	if parked := lifecycleOf(t, h.bus.of(runID), event.TypeParked); len(parked) != 1 {
		t.Errorf("the run parked %d times, want once: an answered hold is not asked again", len(parked))
	}
}

// A hold that is denied, or that nobody answers in time, stops the run: the
// batch it held never runs, the model is not called again, and the reason is
// the person's note, or says nobody answered.
func TestADeniedOrExpiredHoldStopsTheRunWithoutRunningWhatItHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer *event.Answer
		reason string
		by     string
	}{
		{"denied with a note", &event.Answer{Decision: event.DecisionDeny, Note: "Not this month"}, "Not this month", "ana"},
		{"denied", &event.Answer{Decision: event.DecisionDeny}, "A person declined to let it go on", "ana"},
		{"expired", nil, "Nobody answered in time", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHolding(t, newWorld(t),
				sayThenCall("Deploying.", "c1", "deploy", map[string]any{"env": "prod"}),
				agenttest.TextResponse("never"))
			opts := []runtime.Option{}
			janitor := time.Duration(0)
			if tc.answer == nil {
				opts, janitor = append(opts, runtime.WithQuestionTTL(200*time.Millisecond)), 50*time.Millisecond
			}
			rt := h.rt(t, opts...)
			runID := h.accept(t, rt, tag("ops", "evt-1", "ship"))
			hold(t, rt, runID, budgetHold("h1"))
			h.workers(t, rt, 1, janitor)
			q := h.parkedOn(t, rt, runID)
			if tc.answer != nil {
				h.answer(t, rt, runID, q.ID, *tc.answer)
			}

			out := h.result(t, rt, runID)
			if out.Stopped == nil || !strings.HasPrefix(out.Stopped.Reason, tc.reason) || out.Stopped.By != tc.by || out.Text != "Deploying." {
				t.Fatalf("Output = %+v (stopped %+v), want stopped by %q with %q", out, out.Stopped, tc.by, tc.reason)
			}
			if calls := h.model.CallCount(); calls != 1 || len(h.deploy.calls()) != 0 {
				t.Fatalf("the model was called %d times and deploy ran %d times", calls, len(h.deploy.calls()))
			}
			ended(t, h.bus.of(runID))
		})
	}
}

// A host that holds a run while its turn is running is heard at the turn's
// next batch of tool calls: what it was already running finishes, and
// nothing after it runs until the hold is settled.
func TestAHoldMadeMidTurnParksTheNextBatch(t *testing.T) {
	h := newHolding(t, newWorld(t),
		call("c1", "spend", map[string]any{}),
		call("c2", "deploy", map[string]any{"env": "prod"}),
		agenttest.TextResponse("Done."))
	rt := h.rt(t)
	runID := h.accept(t, rt, tag("ops", "evt-1", "ship"))
	h.spend.runID = runID
	h.workers(t, rt, 1, 0)

	q := h.parkedOn(t, rt, runID)
	if q.Kind != event.QuestionHold || q.Hold == nil || q.Hold.ID != "h1" {
		t.Fatalf("question = %+v, want the hold made mid-turn", q)
	}
	if n := len(h.deploy.calls()); n != 0 {
		t.Fatalf("deploy ran %d times after the hold", n)
	}
	h.answer(t, rt, runID, q.ID, event.Answer{Decision: event.DecisionApprove})
	if out := h.result(t, rt, runID); out.Text != "Done." || len(h.deploy.calls()) != 1 {
		t.Fatalf("Output = %+v with %d deploys, want the held batch run once", out, len(h.deploy.calls()))
	}
}

// A turn that answers without another tool call is not held: its last model
// call was made, and there is nothing left for a person to stop.
func TestATurnThatAnswersWithNoToolCallIsNotHeld(t *testing.T) {
	h := newHolding(t, newWorld(t), agenttest.TextResponse("Done."))
	rt := h.rt(t)
	runID := h.accept(t, rt, tag("ops", "evt-1", "ship"))
	hold(t, rt, runID, budgetHold("h1"))
	h.workers(t, rt, 1, 0)
	if out := h.result(t, rt, runID); out.Stopped != nil || out.Text != "Done." {
		t.Fatalf("Output = %+v, want the run completed", out)
	}
}

// Hold records nothing for a run it may not hold: another tenant's, one that
// has ended, or none at all, which read alike. A malformed hold is refused.
func TestAHoldIsRecordedOnlyOnARunOfTheTenantStillGoing(t *testing.T) {
	h := newHolding(t, newWorld(t), agenttest.TextResponse("Done."))
	rt := h.rt(t)
	runID := h.accept(t, rt, tag("ops", "evt-1", "ship"))
	ctx := context.Background()

	if err := rt.Hold(ctx, runtime.HoldRequest{Tenant: "globex", RunID: runID, Hold: budgetHold("h1")}); !errors.Is(err, runtime.ErrNotHeld) {
		t.Errorf("another tenant's hold: %v, want ErrNotHeld", err)
	}
	if err := rt.Hold(ctx, runtime.HoldRequest{Tenant: "acme", RunID: "rnope", Hold: budgetHold("h1")}); !errors.Is(err, runtime.ErrNotHeld) {
		t.Errorf("a hold of no run: %v, want ErrNotHeld", err)
	}
	for _, bad := range []turn.Hold{{}, {ID: "has space"}, {ID: strings.Repeat("x", runtime.MaxHoldID+1)}, {ID: "h", Data: json.RawMessage(`{`)}} {
		if err := rt.Hold(ctx, runtime.HoldRequest{Tenant: "acme", RunID: runID, Hold: bad}); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("hold %+v: %v, want ErrInvalid", bad, err)
		}
	}
	h.workers(t, rt, 1, 0)
	h.result(t, rt, runID)
	if err := rt.Hold(ctx, runtime.HoldRequest{Tenant: "acme", RunID: runID, Hold: budgetHold("h1")}); !errors.Is(err, runtime.ErrNotHeld) {
		t.Errorf("a hold of an ended run: %v, want ErrNotHeld", err)
	}
}

// A hold that names its approvers is answered by them alone, and an answer
// must be approve or deny.
func TestAHoldIsAnsweredOnlyByItsApproversWithApproveOrDeny(t *testing.T) {
	h := newHolding(t, newWorld(t), call("c1", "deploy", map[string]any{"env": "prod"}), agenttest.TextResponse("Done."))
	rt := h.rt(t)
	runID := h.accept(t, rt, tag("ops", "evt-1", "ship"))
	named := budgetHold("h1")
	named.Approvers = []string{"lee"}
	hold(t, rt, runID, named)
	h.workers(t, rt, 1, 0)
	q := h.parkedOn(t, rt, runID)

	ask := func(by string, a event.Answer) error {
		_, _, err := rt.Answer(context.Background(), runtime.AnswerRequest{Tenant: "acme", RunID: runID, QuestionID: q.ID, Answer: a, By: by})
		return err
	}
	if err := ask("ana", event.Answer{Decision: event.DecisionApprove}); !errors.Is(err, runtime.ErrNotAllowed) {
		t.Errorf("ana's answer: %v, want ErrNotAllowed", err)
	}
	if err := ask("lee", event.Answer{Choice: "yes"}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("a choice for a hold: %v, want ErrInvalid", err)
	}
	if err := ask("lee", event.Answer{Decision: event.DecisionApprove}); err != nil {
		t.Fatalf("lee's approval: %v", err)
	}
	if out := h.result(t, rt, runID); out.Text != "Done." {
		t.Fatalf("Output = %+v", out)
	}
}
