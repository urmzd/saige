package native_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// callAgent is a delegation tool whose callee's policy decides, per call,
// whether a person must approve: only a call to "guarded" asks.
type callAgent struct {
	*agenttest.MockTool
}

func (callAgent) Approval(call saigetypes.ToolCallPart) (native.ApprovalRequest, bool) {
	if call.Arguments["agent"] != "guarded" {
		return native.ApprovalRequest{}, false
	}
	return native.ApprovalRequest{
		Prompt:    "Allow ops to call guarded?",
		AskedOf:   "role:admin",
		Approvers: []string{"role:admin", " carol ", "carol"},
	}, true
}

func newCallAgent() callAgent {
	return callAgent{&agenttest.MockTool{Def: saigetypes.ToolDef{Name: "call_agent", Parameters: saigetypes.ParameterSchema{
		Type: "object", Required: []string{"agent"},
		Properties: map[string]saigetypes.PropertyDef{"agent": {Type: "string"}, "task": {Type: "string"}},
	}}, Result: "the callee answered"}}
}

type approvingFixture struct {
	model  *agenttest.ScriptedProvider
	tool   callAgent
	runner *native.Runner
}

func newApprovingFixture(t *testing.T, approvals []string, script ...[]saigetypes.Delta) *approvingFixture {
	t.Helper()
	f := &approvingFixture{model: &agenttest.ScriptedProvider{Responses: script}, tool: newCallAgent()}
	rev := offlineRevision(1, "You delegate.")
	rev.Spec.Tools = []string{"call_agent"}
	rev.Spec.Approvals = approvals
	r, err := native.New(rev,
		native.WithTools(native.StaticTools(f.tool)),
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return f.model, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.runner = r
	return f
}

func (f *approvingFixture) run(t *testing.T, turn native.Turn) native.Result {
	t.Helper()
	res, err := f.runner.Run(context.Background(), turn, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func callTo(agent string) []saigetypes.Delta {
	return toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "call_agent", Arguments: map[string]any{"agent": agent, "task": "t"}})
}

// A call the tool does not ask about runs at once, with no question.
func TestAnUnguardedCallRunsWithoutAQuestion(t *testing.T) {
	t.Parallel()
	f := newApprovingFixture(t, nil, callTo("open"), agenttest.TextResponse("Done."))
	res := f.run(t, native.Turn{Text: "go", Park: true})
	if res.Parked != nil {
		t.Fatalf("an unguarded call parked on %+v", res.Parked.Question)
	}
	if len(f.tool.Calls) != 1 || res.Text != "Done." {
		t.Fatalf("call_agent ran %d times, text %q; want once and Done.", len(f.tool.Calls), res.Text)
	}
}

// A call the tool asks about parks as an approval carrying the tool's own
// prompt, hint and approvers, and nothing runs until it is answered.
func TestAGuardedCallParksAsAnApprovalWithItsApprovers(t *testing.T) {
	t.Parallel()
	f := newApprovingFixture(t, nil, callTo("guarded"))
	res := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a1"})
	if res.Parked == nil {
		t.Fatal("a guarded call did not park")
	}
	q := res.Parked.Question
	if q.Kind != event.QuestionApproval || q.Tool == nil || q.Tool.Name != "call_agent" || q.Tool.CallID != "c1" || res.Parked.CallID != "c1" {
		t.Fatalf("question is %+v, want an approval of call_agent c1", q)
	}
	if q.Prompt != "Allow ops to call guarded?" || q.AskedOf != "role:admin" || strings.Join(q.Approvers, ",") != "role:admin,carol" || q.SelfApproval {
		t.Errorf("question carries prompt %q asked_of %q approvers %v self %v", q.Prompt, q.AskedOf, q.Approvers, q.SelfApproval)
	}
	if q.Default == nil || q.Default.Decision != event.DecisionDeny || !q.Default.Expired || q.ExpiresAt.IsZero() {
		t.Errorf("question default %+v expiry %v, want a deny on expiry", q.Default, q.ExpiresAt)
	}
	if len(f.tool.Calls) != 0 {
		t.Fatalf("call_agent ran %d times before anyone answered", len(f.tool.Calls))
	}
}

// The answer decides the call: an approval runs it, a denial and an expiry do
// not, and the model is told why.
func TestAGuardedCallRunsOnlyOnApproval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(q event.Question) event.Answer
		runs   bool
		told   string
	}{
		{"approved", func(q event.Question) event.Answer {
			return event.Answer{QuestionID: q.ID, Decision: event.DecisionApprove, By: "carol"}
		}, true, "the callee answered"},
		{"denied", func(q event.Question) event.Answer {
			return event.Answer{QuestionID: q.ID, Decision: event.DecisionDeny, By: "carol", Note: "not this week"}
		}, false, "not this week"},
		{"expired", func(q event.Question) event.Answer { return *q.Default }, false, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newApprovingFixture(t, nil, callTo("guarded"), agenttest.TextResponse("Over."))
			parked := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a1"})
			if parked.Parked == nil {
				t.Fatal("did not park")
			}
			resume := *parked.Parked
			resume.Answers = map[string]event.Answer{"c1": tc.answer(parked.Parked.Question)}
			res := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a2", Resume: &resume})
			if res.Parked != nil {
				t.Fatalf("the continuation parked again on %+v", res.Parked.Question)
			}
			if ran := len(f.tool.Calls) == 1; ran != tc.runs || len(f.tool.Calls) > 1 {
				t.Fatalf("call_agent ran %d times, want runs=%v", len(f.tool.Calls), tc.runs)
			}
			if !messagesContain(f.model.Requests()[1].Messages, tc.told) {
				t.Errorf("the model was not told %q: %s", tc.told, renderMessages(f.model.Requests()[1].Messages))
			}
		})
	}
}

// A turn that cannot park refuses a guarded call rather than running it.
func TestAGuardedCallIsRefusedWhereNobodyCanBeAsked(t *testing.T) {
	t.Parallel()
	f := newApprovingFixture(t, nil, callTo("guarded"), agenttest.TextResponse("Could not."))
	res := f.run(t, native.Turn{Text: "go"})
	if res.Parked != nil || len(f.tool.Calls) != 0 {
		t.Fatalf("parked %v, call_agent ran %d times; want neither", res.Parked != nil, len(f.tool.Calls))
	}
}

// A tool the spec also lists in Approvals is asked about once: with the
// tool's question when it asks, with the spec's otherwise.
func TestASpecApprovalAndAPerCallApprovalAskOnce(t *testing.T) {
	t.Parallel()
	f := newApprovingFixture(t, []string{"call_agent"}, callTo("guarded"), agenttest.TextResponse("Over."))
	parked := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a1"})
	if parked.Parked == nil || parked.Parked.Question.Prompt != "Allow ops to call guarded?" || len(parked.Parked.Question.Approvers) == 0 {
		t.Fatalf("parked on %+v, want the per-call question", parked.Parked)
	}
	resume := *parked.Parked
	resume.Answers = map[string]event.Answer{"c1": {QuestionID: parked.Parked.Question.ID, Decision: event.DecisionApprove}}
	if res := f.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a2", Resume: &resume}); res.Parked != nil || len(f.tool.Calls) != 1 {
		t.Fatalf("after approval: parked %v, ran %d times; want no park and one run", res.Parked != nil, len(f.tool.Calls))
	}

	open := newApprovingFixture(t, []string{"call_agent"}, callTo("open"))
	res := open.run(t, native.Turn{Text: "go", Park: true, Accounting: "r/a1"})
	if res.Parked == nil || res.Parked.Question.Approvers != nil || len(open.tool.Calls) != 0 {
		t.Fatalf("an unguarded call to a tool the spec guards: parked %+v, ran %d", res.Parked, len(open.tool.Calls))
	}
}

// expiringCall asks for approval of every call, waiting as long as it says.
type expiringCall struct {
	*agenttest.MockTool
	wait time.Duration
}

func (e expiringCall) Approval(saigetypes.ToolCallPart) (native.ApprovalRequest, bool) {
	return native.ApprovalRequest{Approvers: []string{"role:admin"}, ExpiresIn: e.wait}, true
}

// A tool's own wait sets when its question expires, never past the cap.
func TestAnApprovalWaitsAsLongAsTheToolSays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wait, want time.Duration
	}{
		{2 * time.Hour, 2 * time.Hour},
		{30 * 24 * time.Hour, native.MaxApprovalWait},
	} {
		tool := expiringCall{newCallAgent().MockTool, tc.wait}
		model := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{callTo("any")}}
		rev := offlineRevision(1, "You delegate.")
		rev.Spec.Tools = []string{"call_agent"}
		r, err := native.New(rev,
			native.WithTools(native.StaticTools(tool)),
			native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }))
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Run(context.Background(), native.Turn{Text: "go", Park: true, Accounting: "r/e1"}, nil)
		if err != nil || res.Parked == nil {
			t.Fatalf("Run: %v, parked %v", err, res.Parked)
		}
		q := res.Parked.Question
		if got := q.ExpiresAt.Sub(q.At); got != tc.want {
			t.Errorf("wait %v: question expires after %v, want %v", tc.wait, got, tc.want)
		}
	}
}
