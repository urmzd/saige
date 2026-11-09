package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// fanout is a lead that delegates to ops three times in one message, then
// answers with every result it was given, in the order they came.
type fanout struct{ calls atomic.Int32 }

func (m *fanout) Stream(_ context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	m.calls.Add(1)
	var deltas []saigetypes.Delta
	if results := resultsIn(req.Messages); len(results) > 0 {
		deltas = agenttest.TextResponse("Lead got: " + strings.Join(results, " | "))
	} else {
		tool := runtime.DelegateToolName("ops")
		for i, x := range []string{"a", "b", "c"} {
			call := saigetypes.ToolCallPart{ID: "d" + x, Name: tool, Arguments: map[string]any{"text": "deploy " + x}}
			deltas = append(deltas,
				saigetypes.PartStart{Index: i, Kind: saigetypes.KindToolCall, ID: call.ID, Name: call.Name},
				saigetypes.PartEnd{Index: i, Part: call})
		}
	}
	ch := make(chan saigetypes.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// resultsIn is the text of every tool result in a conversation, in order.
func resultsIn(msgs []saigetypes.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, p := range saigetypes.PartsOf(m) {
			if tr, ok := p.(saigetypes.ToolResultPart); ok {
				out = append(out, tr.Text())
			}
		}
	}
	return out
}

// fanoutWorld is a world where "lead" delegates to "ops" three times at once,
// with delegations as child runs, and each ops child asks a person which env.
func fanoutWorld(t *testing.T, w *world) (*runtime.Runtime, *parking, *fanout) {
	t.Helper()
	p := newParking(t, w)
	w.specs.set(native.Spec{
		Name: "lead", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "Lead.",
		Access: native.Access{Spawn: []string{"ops"}},
	})
	lead := &fanout{}
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{
		"lead": lead,
		"ops": routed{tool: native.AskPersonToolName, args: map[string]any{
			"question": "Which env?", "options": []any{"prod", "staging", "dev"},
		}, done: "Child chose"},
	}, runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
		return native.StaticTools(p.deploy), nil
	}))
	return p.rt(t, runtime.WithChildRuns()), p, lead
}

// batchParked waits until a run announces a park on n questions at once and
// returns them.
func (w *world) batchParked(t *testing.T, runID string, n int) []event.Question {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range w.bus.of(runID) {
			if e.Type != event.TypeParked {
				continue
			}
			body, err := decode[event.Parked](e)
			if err == nil && len(body.Questions) == n {
				if body.Question == nil || body.Question.ID != body.Questions[0].ID {
					t.Fatalf("the park's question is %+v, want the first of its questions", body.Question)
				}
				return body.Questions
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never parked on %d questions: %v", runID, n, kindsOf(w.bus.of(runID)))
	return nil
}

func pendingIDs(t *testing.T, rt *runtime.Runtime, runID string) []string {
	t.Helper()
	qs, err := rt.PendingQuestions(context.Background(), "acme", runID)
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
	}
	var out []string
	for _, q := range qs {
		out = append(out, q.ID)
	}
	return out
}

// A message that delegates three times parks its run once, on all three, and
// every child run starts before anything is answered: the three children
// are parked on their own questions side by side, which no child could be if
// each started only after the one before it answered. Each child's answer
// arrives on its own, in any order, and the parent resumes once, when the
// last is in, with every result in call order.
func TestABatchOfDelegationsStartsEveryChildAndParksTheParentOnce(t *testing.T) {
	w := newWorld(t)
	rt, p, lead := fanoutWorld(t, w)
	p.workers(t, rt, 4, 0)
	parent := p.accept(t, rt, tag("lead", "evt-1", "ship"))

	waits := p.batchParked(t, parent, 3)
	child := map[string]string{}
	for _, q := range waits {
		var args struct{ Text string }
		if q.Kind != event.QuestionDelegation || q.ChildRunID == "" || q.Tool == nil || json.Unmarshal(q.Tool.Arguments, &args) != nil {
			t.Fatalf("the parent waits on %+v, want a child run of ops", q)
		}
		child[args.Text] = q.ChildRunID
	}
	if len(child) != 3 || child["deploy a"] == child["deploy b"] || child["deploy b"] == child["deploy c"] {
		t.Fatalf("the parent waits on children %v, want one per call", child)
	}
	if got := pendingIDs(t, rt, parent); len(got) != 3 {
		t.Fatalf("the parent is parked on %d questions, want 3", len(got))
	}
	// Every child is running before any has an answer.
	asked := map[string]event.Question{}
	for text, id := range child {
		asked[text] = pendingOf(t, rt, id, event.QuestionChoice)
	}

	// The answers arrive out of call order. Until the last, the parent stays
	// parked on the rest, and says nothing.
	for i, step := range []struct{ text, choice string }{{"deploy c", "staging"}, {"deploy a", "prod"}, {"deploy b", "dev"}} {
		p.answer(t, rt, child[step.text], asked[step.text].ID, event.Answer{Choice: step.choice})
		if out := p.result(t, rt, child[step.text]); out.Text != "Child chose "+step.choice {
			t.Fatalf("child %q answered %q", step.text, out.Text)
		}
		if i == 2 {
			break
		}
		if got := pendingIDs(t, rt, parent); len(got) != 2-i {
			t.Fatalf("after %d answers the parent waits on %d questions, want %d", i+1, len(got), 2-i)
		}
		if n := len(lifecycleOf(t, p.bus.of(parent), event.TypeResumed)); n != 0 {
			t.Fatalf("the parent resumed after %d of 3 answers", i+1)
		}
	}

	out := p.result(t, rt, parent)
	if want := "Lead got: Child chose prod | Child chose dev | Child chose staging"; out.Text != want {
		t.Fatalf("parent answered %q, want %q", out.Text, want)
	}
	if n := lead.calls.Load(); n != 2 {
		t.Errorf("the lead's model was called %d times, want 2", n)
	}
	if parked := lifecycleOf(t, p.bus.of(parent), event.TypeParked); len(parked) != 1 || len(parked[0].Questions) != 3 || parked[0].Reason != "waiting on sessions" {
		t.Errorf("the parent announced %d parks (%+v), want one on all three", len(parked), parked)
	}
	resumed := lifecycleOf(t, p.bus.of(parent), event.TypeResumed)
	if len(resumed) != 1 || len(resumed[0].Answers) != 3 || resumed[0].Answer == nil || resumed[0].Answer.QuestionID != waits[0].ID {
		t.Fatalf("the parent resumed %d times (%+v), want once with three answers", len(resumed), resumed)
	}
	for i, a := range resumed[0].Answers {
		if a.QuestionID != waits[i].ID || a.RunID != waits[i].ChildRunID {
			t.Errorf("answer %d is %+v, want the answer of %s", i, a, waits[i].ChildRunID)
		}
	}
	children := 0
	for _, e := range p.bus.of(parent) {
		if e.Type == event.TypeUsageChild {
			children++
		}
	}
	if children != 3 {
		t.Errorf("the parent rolled up %d child runs, want 3", children)
	}
}

// Stopping a parent parked on a batch of delegations stops every child, with
// the same words, and closes every question of the park.
func TestStoppingAParentWaitingOnABatchStopsEveryChild(t *testing.T) {
	w := newWorld(t)
	rt, p, _ := fanoutWorld(t, w)
	p.workers(t, rt, 4, 0)
	parent := p.accept(t, rt, tag("lead", "evt-1", "ship"))
	waits := p.batchParked(t, parent, 3)
	for _, q := range waits {
		pendingOf(t, rt, q.ChildRunID, event.QuestionChoice)
	}

	p.stop(t, rt, "acme", parent, "Not today", "U1")
	if out := p.result(t, rt, parent); out.Stopped == nil || out.Stopped.Reason != "Not today" {
		t.Fatalf("parent Output = %+v", out)
	}
	for _, q := range waits {
		if out := p.result(t, rt, q.ChildRunID); out.Stopped == nil || out.Stopped.Reason != "Not today" {
			t.Errorf("child %s Output = %+v", q.ChildRunID, out)
		}
	}
	if got := pendingIDs(t, rt, parent); len(got) != 0 {
		t.Errorf("the stopped parent still waits on %v", got)
	}
}

// A batch with a call that waits on nothing and two questions parks once on
// both questions, and runs nothing. The questions are answered one at a
// time, out of order; the run resumes once both are in, runs the call that
// did not wait exactly once, and gives the model every result in call order.
// A replay after the resume asks, calls and runs nothing again.
func TestAMixedBatchParksOnceAndRunsItsOtherCallOnce(t *testing.T) {
	w := newWorld(t)
	batch := []saigetypes.Delta{}
	for i, c := range []saigetypes.ToolCallPart{
		{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}},
		{ID: "q1", Name: native.AskPersonToolName, Arguments: map[string]any{"question": "Which region?", "options": []any{"eu", "us"}}},
		{ID: "q2", Name: native.AskPersonToolName, Arguments: map[string]any{"question": "Why?"}},
	} {
		batch = append(batch,
			saigetypes.PartStart{Index: i, Kind: saigetypes.KindToolCall, ID: c.ID, Name: c.Name},
			saigetypes.PartEnd{Index: i, Part: c})
	}
	// deploy needs no approval here: it is the call that waits on nothing.
	p := newParkingWith(t, w, func(s *native.Spec) { s.Approvals = nil }, batch, agenttest.TextResponse("Done."))
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
	runID := p.accept(t, rt, tag("ops", "evt-1", "ship"))

	qs := p.batchParked(t, runID, 2)
	if qs[0].Kind != event.QuestionChoice || qs[1].Kind != event.QuestionReply {
		t.Fatalf("the run waits on %+v, want the choice then the reply", qs)
	}
	if got := pendingIDs(t, rt, runID); !slices.Equal(got, []string{qs[0].ID, qs[1].ID}) {
		t.Fatalf("pending questions are %v, want both, oldest first", got)
	}

	p.answer(t, rt, runID, qs[1].ID, event.Answer{Text: "audit"})
	// The answer wakes the run, which finds the other question open and
	// parks again without a word.
	time.Sleep(300 * time.Millisecond)
	if n := len(lifecycleOf(t, p.bus.of(runID), event.TypeResumed)); n != 0 || len(p.deploy.calls()) != 0 || p.model.CallCount() != 1 {
		t.Fatalf("after one of two answers: %d resumes, deploy %d, model %d; want 0, 0, 1", n, len(p.deploy.calls()), p.model.CallCount())
	}
	if got := pendingIDs(t, rt, runID); !slices.Equal(got, []string{qs[0].ID}) {
		t.Fatalf("pending questions are %v, want the unanswered one", got)
	}
	p.answer(t, rt, runID, qs[0].ID, event.Answer{Choice: "eu"})

	out := p.result(t, rt, runID)
	if out.Text != "Done." {
		t.Fatalf("Output = %+v", out)
	}
	if p.model.CallCount() != 2 || len(p.deploy.calls()) != 1 {
		t.Fatalf("model %d calls, deploy %d: want 2 and 1", p.model.CallCount(), len(p.deploy.calls()))
	}
	want := []string{"deployed to prod", "eu", "audit"}
	if got := resultsIn(p.model.Requests()[1].Messages); !slices.Equal(got, want) {
		t.Errorf("the model saw results %v, want %v", got, want)
	}

	// The broker outage failed the attempt that resumed; the next replayed it.
	offered := w.threads.offered()
	if len(offered) != 2 || !reflect.DeepEqual(offered[0], offered[1]) {
		t.Errorf("the journal was offered %d exchanges, want the same one twice", len(offered))
	}
	if parked := lifecycleOf(t, p.bus.of(runID), event.TypeParked); len(parked) != 1 {
		t.Errorf("the park was announced %d times, want once", len(parked))
	}
	// The attempt that resumed and the one that replayed it each open with
	// both answers.
	resumed := lifecycleOf(t, p.bus.of(runID), event.TypeResumed)
	if len(resumed) == 0 {
		t.Error("the run never announced that it resumed")
	}
	for _, r := range resumed {
		if len(r.Answers) != 2 || r.Answers[0].Choice != "eu" || r.Answers[1].Text != "audit" {
			t.Errorf("attempt %d resumed with %+v, want both answers in call order", r.Attempt, r.Answers)
		}
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
	if questions != 2 {
		t.Errorf("the ledger holds %d question records, want 2", questions)
	}
}
