package native_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// parkFixture is an agent with a deploy tool that needs approval, a lookup
// tool that does not, and ask_person, over one scripted model shared by every
// turn built from it, so a continuation reads the script where the parked
// turn stopped.
type parkFixture struct {
	model  *agenttest.ScriptedProvider
	deploy *agenttest.MockTool
	lookup *agenttest.MockTool
	runner *native.Runner
}

func newParkFixture(t *testing.T, approvals []string, script ...[]saigetypes.Delta) *parkFixture {
	t.Helper()
	f := &parkFixture{
		model: &agenttest.ScriptedProvider{Responses: script},
		deploy: &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "deploy", Parameters: saigetypes.ParameterSchema{
			Type: "object", Required: []string{"env"},
			Properties: map[string]saigetypes.PropertyDef{"env": {Type: "string"}},
		}}, Result: "deployed"},
		lookup: &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "lookup"}, Result: "found"},
	}
	rev := offlineRevision(1, "You deploy things.")
	rev.Spec.Tools = []string{"deploy", "lookup", native.AskPersonToolName}
	rev.Spec.Approvals = approvals
	r, err := native.New(rev,
		native.WithTools(native.StaticTools(f.deploy, f.lookup)),
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return f.model, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.runner = r
	return f
}

func (f *parkFixture) run(t *testing.T, turn native.Turn) ([]event.Body, native.Result) {
	t.Helper()
	var got []event.Body
	res, err := f.runner.Run(context.Background(), turn, func(_ context.Context, c event.Body) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got, res
}

func toolCalls(parts ...saigetypes.ToolCallPart) []saigetypes.Delta {
	var out []saigetypes.Delta
	for i, p := range parts {
		out = append(out,
			saigetypes.PartStart{Index: i, Kind: saigetypes.KindToolCall, ID: p.ID, Name: p.Name},
			saigetypes.PartEnd{Index: i, Part: p})
	}
	return out
}

func textThen(text string, rest []saigetypes.Delta) []saigetypes.Delta {
	out := saigetypes.PartDeltas(0, saigetypes.Text(text))
	for _, d := range rest {
		switch v := d.(type) {
		case saigetypes.PartStart:
			v.Index++
			d = v
		case saigetypes.PartEnd:
			v.Index++
			d = v
		}
		out = append(out, d)
	}
	return out
}

// personTool runs with the credential of the person the turn acts for, as a
// person-scoped provider's tools do.
type personTool struct{ *agenttest.MockTool }

func (personTool) ActsForPerson() bool { return true }

// An approval of a call that runs as the person the turn acts for says so,
// through the marker a spec's approvals add, so the runtime lets only that
// person edit it. An approval of any other tool does not.
func TestAnApprovalSaysWhetherTheCallActsAsThePerson(t *testing.T) {
	t.Parallel()
	for _, person := range []bool{true, false} {
		deploy := &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "deploy", Parameters: saigetypes.ParameterSchema{
			Type: "object", Properties: map[string]saigetypes.PropertyDef{"env": {Type: "string"}},
		}}, Result: "deployed"}
		var tool saigetypes.Tool = deploy
		if person {
			tool = personTool{deploy}
		}
		model := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
			toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}}),
		}}
		rev := offlineRevision(1, "You deploy things.")
		rev.Spec.Tools, rev.Spec.Approvals = []string{"deploy"}, []string{"deploy"}
		r, err := native.New(rev, native.WithTools(native.StaticTools(tool)),
			native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := r.Run(context.Background(), native.Turn{Text: "ship it", Park: true, Accounting: "r/a1"},
			func(context.Context, event.Body) error { return nil })
		if err != nil || res.Parked == nil || res.Parked.Question.Tool == nil {
			t.Fatalf("person %v: the turn did not park on an approval: %+v, %v", person, res, err)
		}
		if got := res.Parked.Question.Tool.ActsForPerson; got != person {
			t.Errorf("person %v: the question says acts_for_person %v", person, got)
		}
		if deploy.CallCount() != 0 {
			t.Errorf("person %v: deploy ran while the turn parked", person)
		}
	}
}

// An approval parks the turn before anything in the model's batch runs, the
// tool that needed no approval included, and the question carries the call.
func TestAnApprovalParksTheTurnBeforeItsBatchRuns(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		textThen("Deploying now.", toolCalls(
			saigetypes.ToolCallPart{ID: "c1", Name: "lookup", Arguments: map[string]any{}},
			saigetypes.ToolCallPart{ID: "c2", Name: "deploy", Arguments: map[string]any{"env": "prod"}},
		)))

	got, res := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a1"})

	if res.Parked == nil {
		t.Fatalf("the turn did not park: %+v", res)
	}
	q := res.Parked.Question
	if q.Kind != event.QuestionApproval || q.Tool == nil || q.Tool.Name != "deploy" || q.Tool.CallID != "c2" {
		t.Fatalf("question is %+v, want an approval of deploy c2", q)
	}
	if !strings.Contains(string(q.Tool.Arguments), `"prod"`) || q.ExpiresAt.IsZero() || q.Default == nil || q.Default.Decision != event.DecisionDeny {
		t.Errorf("question is %+v, want the arguments, an expiry and a deny default", q)
	}
	if len(f.deploy.Calls) != 0 || len(f.lookup.Calls) != 0 {
		t.Errorf("deploy ran %d and lookup %d times while the turn parked, want 0 and 0", len(f.deploy.Calls), len(f.lookup.Calls))
	}
	if hasKind(got, event.TypeToolDone) {
		t.Errorf("chunks %v report a tool finishing in a batch that never ran", kinds(got))
	}
	if res.Text != "Deploying now." {
		t.Errorf("text is %q, want what was said before the question", res.Text)
	}
	if f.model.CallCount() != 1 {
		t.Errorf("the model was called %d times, want 1", f.model.CallCount())
	}
}

// The continuation runs the parked batch with the answer, sends the model the
// conversation it would have seen had nobody waited, byte for byte, and does
// not publish or count the recorded call again.
func TestAnApprovedContinuationRunsTheBatchAndKeepsThePromptStable(t *testing.T) {
	t.Parallel()
	batch := textThen("Deploying now.", toolCalls(
		saigetypes.ToolCallPart{ID: "c1", Name: "lookup", Arguments: map[string]any{}},
		saigetypes.ToolCallPart{ID: "c2", Name: "deploy", Arguments: map[string]any{"env": "prod"}},
	))
	done := agenttest.TextResponse("Deployed to prod.")

	// The run nobody had to wait for: no approvals.
	straight := newParkFixture(t, nil, batch, done)
	_, want := straight.run(t, native.Turn{Text: "ship it", Park: true})
	if want.Parked != nil || want.Text != "Deploying now.Deployed to prod." {
		t.Fatalf("the uninterrupted run gave %+v", want)
	}

	f := newParkFixture(t, []string{"deploy"}, batch, done)
	_, parked := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a1"})
	if parked.Parked == nil {
		t.Fatal("the turn did not park")
	}
	resume := *parked.Parked
	resume.Answers = map[string]event.Answer{"c2": {QuestionID: parked.Parked.Question.ID, Decision: event.DecisionApprove, By: "ana"}}
	got, res := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a2", Resume: &resume})

	if res.Parked != nil {
		t.Fatalf("the continuation parked again on %+v", res.Parked.Question)
	}
	if len(f.deploy.Calls) != 1 || len(f.lookup.Calls) != 1 {
		t.Errorf("deploy ran %d and lookup %d times, want once each", len(f.deploy.Calls), len(f.lookup.Calls))
	}
	if res.Text != "Deployed to prod." {
		t.Errorf("continuation text is %q, want only what it said", res.Text)
	}
	if f.model.CallCount() != 2 {
		t.Fatalf("the model was called %d times, want 2: the recorded call is not sent", f.model.CallCount())
	}
	for _, c := range got {
		if c.Type() == event.TypeToolCall {
			tc, _ := decodeBody[event.ToolCall](c)
			if tc.Phase == event.PhaseRequested {
				t.Errorf("the continuation published the recorded call's request for %s again", tc.Name)
			}
		}
		if c.Type() == event.TypeText && strings.Contains(string(raw(c)), "Deploying now.") {
			t.Error("the continuation published the recorded call's text again")
		}
	}
	if n := countKind(got, event.TypeUsageCall); n != 1 {
		t.Errorf("the continuation reported %d model calls, want 1", n)
	}
	sent := renderMessages(f.model.Requests()[1].Messages)
	if wantSent := renderMessages(straight.model.Requests()[1].Messages); sent != wantSent {
		t.Errorf("the continuation sent\n%s\nwant what the uninterrupted run sent\n%s", sent, wantSent)
	}
}

// A denial never runs the tool, and the model is told why.
func TestADeniedApprovalIsNotRunAndTheModelSeesTheReason(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}}),
		agenttest.TextResponse("Not deployed."))
	_, parked := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a1"})
	resume := *parked.Parked
	resume.Answers = map[string]event.Answer{"c1": {Decision: event.DecisionDeny, Note: "freeze week"}}
	_, res := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a2", Resume: &resume})

	if len(f.deploy.Calls) != 0 {
		t.Errorf("deploy ran %d times after a denial", len(f.deploy.Calls))
	}
	if !messagesContain(f.model.Requests()[1].Messages, "freeze week") {
		t.Errorf("the model was not told the reason: %s", renderMessages(f.model.Requests()[1].Messages))
	}
	if res.Text != "Not deployed." {
		t.Errorf("text is %q", res.Text)
	}
}

// Edited arguments are what the tool runs with.
func TestAnApprovalWithEditedArgumentsRunsTheEditedCall(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}}),
		agenttest.TextResponse("Deployed."))
	_, parked := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a1"})
	resume := *parked.Parked
	resume.Answers = map[string]event.Answer{"c1": {Decision: event.DecisionApprove, Arguments: json.RawMessage(`{"env":"staging"}`)}}
	f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a2", Resume: &resume})

	if len(f.deploy.Calls) != 1 || f.deploy.Calls[0]["env"] != "staging" {
		t.Errorf("deploy ran with %v, want env staging once", f.deploy.Calls)
	}
}

// ask_person parks as a choice when it offers options and as a reply when it
// does not, and its result is the answer.
func TestAskPersonParksAsAChoiceOrAReplyAndReturnsTheAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   map[string]any
		kind   string
		answer event.Answer
		want   string
	}{
		{"choice", map[string]any{"question": "Which region?", "options": []any{"eu", "us"}, "default": "eu"}, event.QuestionChoice, event.Answer{Choice: "us"}, "us"},
		{"reply", map[string]any{"question": "What should the release be called?", "asked_of": "release-manager"}, event.QuestionReply, event.Answer{Text: "Aurora"}, "Aurora"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newParkFixture(t, nil,
				toolCalls(saigetypes.ToolCallPart{ID: "q1", Name: native.AskPersonToolName, Arguments: tc.args}),
				agenttest.TextResponse("Thanks."))
			_, parked := f.run(t, native.Turn{Text: "release", Park: true, Accounting: "r/a1"})
			if parked.Parked == nil {
				t.Fatal("ask_person did not park the turn")
			}
			q := parked.Parked.Question
			if q.Kind != tc.kind || q.Prompt != tc.args["question"] {
				t.Fatalf("question is %+v, want a %s", q, tc.kind)
			}
			if tc.kind == event.QuestionChoice && (len(q.Options) != 2 || q.Default == nil || q.Default.Choice != "eu") {
				t.Errorf("choice is %+v, want two options and the default eu", q)
			}
			if tc.kind == event.QuestionReply && q.AskedOf != "release-manager" {
				t.Errorf("asked_of is %q", q.AskedOf)
			}
			resume := *parked.Parked
			resume.Answers = map[string]event.Answer{"q1": tc.answer}
			f.run(t, native.Turn{Text: "release", Park: true, Accounting: "r/a2", Resume: &resume})
			if !messagesContain(f.model.Requests()[1].Messages, tc.want) {
				t.Errorf("the model did not see the answer %q: %s", tc.want, renderMessages(f.model.Requests()[1].Messages))
			}
		})
	}
}

// Two questions in one batch park twice: the second continuation carries both
// answers, and the batch runs once, after both.
func TestABatchWithTwoQuestionsParksOncePerQuestion(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		toolCalls(
			saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}},
			saigetypes.ToolCallPart{ID: "q1", Name: native.AskPersonToolName, Arguments: map[string]any{"question": "Why?"}},
		),
		agenttest.TextResponse("Done."))
	_, first := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a1"})
	if first.Parked == nil || first.Parked.CallID != "c1" {
		t.Fatalf("first park is %+v, want the deploy approval", first.Parked)
	}
	resume := *first.Parked
	resume.Answers = map[string]event.Answer{"c1": {Decision: event.DecisionApprove}}
	_, second := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a2", Resume: &resume})
	if second.Parked == nil || second.Parked.CallID != "q1" {
		t.Fatalf("second park is %+v, want the question", second.Parked)
	}
	if len(f.deploy.Calls) != 0 {
		t.Fatal("deploy ran before the batch's second question was answered")
	}
	again := *second.Parked
	again.Answers["q1"] = event.Answer{Text: "audit"}
	_, res := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a3", Resume: &again})
	if res.Parked != nil || res.Text != "Done." || len(f.deploy.Calls) != 1 {
		t.Errorf("third turn gave %+v with deploy run %d times", res, len(f.deploy.Calls))
	}
	if f.model.CallCount() != 2 {
		t.Errorf("the model was called %d times, want 2", f.model.CallCount())
	}
}

// A turn that may not park refuses approvals and tells the model ask_person
// cannot be answered, so it carries on instead of waiting forever.
func TestATurnThatCannotParkRefusesInsteadOfWaiting(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		toolCalls(
			saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}},
			saigetypes.ToolCallPart{ID: "q1", Name: native.AskPersonToolName, Arguments: map[string]any{"question": "Sure?"}},
		),
		agenttest.TextResponse("Could not."))
	_, res := f.run(t, native.Turn{Text: "go"})
	if res.Parked != nil || res.Text != "Could not." || len(f.deploy.Calls) != 0 {
		t.Errorf("result %+v, deploy ran %d times; want a finished turn and no deploy", res, len(f.deploy.Calls))
	}
	if !messagesContain(f.model.Requests()[1].Messages, "no person can") {
		t.Errorf("the model was not told: %s", renderMessages(f.model.Requests()[1].Messages))
	}
}

// A parked turn's record survives JSON: what a runtime stores is what it
// continues from.
func TestAParkedTurnSurvivesItsRecord(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		textThen("First I look.", toolCalls(saigetypes.ToolCallPart{ID: "c0", Name: "lookup", Arguments: map[string]any{}})),
		toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}}),
		agenttest.TextResponse("Deployed."))
	_, parked := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a1"})
	raw, err := json.Marshal(parked.Parked)
	if err != nil {
		t.Fatal(err)
	}
	var back native.Parked
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Transcript) != 2 {
		t.Fatalf("transcript has %d messages, want the first call and its result", len(back.Transcript))
	}
	back.Answers = map[string]event.Answer{"c1": {Decision: event.DecisionApprove}}
	_, res := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a2", Resume: &back})
	if res.Text != "Deployed." || len(f.lookup.Calls) != 1 || len(f.deploy.Calls) != 1 {
		t.Errorf("continuation gave %q, lookup %d, deploy %d; want each tool once", res.Text, len(f.lookup.Calls), len(f.deploy.Calls))
	}
	if !messagesContain(f.model.Requests()[2].Messages, "First I look.") {
		t.Error("the continuation's model call is missing the conversation before the park")
	}
}

func countKind(chunks []event.Body, k string) int {
	n := 0
	for _, c := range chunks {
		if c.Type() == k {
			n++
		}
	}
	return n
}

// A continuation's tool events carry the call's arguments like any turn's,
// though the model message that asked for the call is not streamed again:
// the executing event has what the model asked for, the done event what the
// approval let the tool run with.
func TestAContinuationsToolEventsCarryTheParkedCallsArguments(t *testing.T) {
	t.Parallel()
	f := newParkFixture(t, []string{"deploy"},
		toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "deploy", Arguments: map[string]any{"env": "prod"}}),
		agenttest.TextResponse("Deployed."))
	_, parked := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a1"})
	resume := *parked.Parked
	resume.Answers = map[string]event.Answer{"c1": {Decision: event.DecisionApprove, Arguments: json.RawMessage(`{"env":"staging"}`)}}
	got, _ := f.run(t, native.Turn{Text: "ship it", Park: true, Accounting: "r/a2", Resume: &resume})

	var executing, done string
	for _, c := range got {
		switch c.Type() {
		case event.TypeToolCall:
			var body event.ToolCall
			if err := json.Unmarshal(raw(c), &body); err != nil {
				t.Fatal(err)
			}
			if body.Phase == event.PhaseExecuting {
				executing = string(body.Arguments)
			}
		case event.TypeToolDone:
			var body event.ToolDone
			if err := json.Unmarshal(raw(c), &body); err != nil {
				t.Fatal(err)
			}
			done = string(body.Arguments)
		}
	}
	if executing != `{"env":"prod"}` {
		t.Errorf("the continuation's executing event carries %q, want the model's arguments", executing)
	}
	if done != `{"env":"staging"}` {
		t.Errorf("the continuation's done event carries %q, want the edited arguments", done)
	}
}
