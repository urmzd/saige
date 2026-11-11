package native_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// A turn that may park ends at the first batch of tool calls once the host
// holds it, runs nothing in the batch, and continues with the batch once the
// hold is approved. A turn that may not park is never held.
func TestAHostsHoldParksTheNextBatchOnlyWhereTheTurnMayPark(t *testing.T) {
	holds := func(context.Context) []native.Hold {
		return []native.Hold{{ID: "h1", Topic: "budget", Prompt: "Grant more?", Approvers: []string{"role:billing"}}}
	}
	batch := toolCalls(saigetypes.ToolCallPart{ID: "c1", Name: "lookup", Arguments: map[string]any{}})

	f := newParkFixture(t, nil, batch, agenttest.TextResponse("Done."))
	_, res := f.run(t, native.Turn{Text: "go", Park: true, Holds: holds})
	if res.Parked == nil {
		t.Fatalf("Result = %+v, want parked on the hold", res)
	}
	q := res.Parked.Question
	if q.Kind != event.QuestionHold || q.Hold == nil || q.Hold.ID != "h1" || q.Prompt != "Grant more?" ||
		len(q.Approvers) != 1 || q.Default == nil || q.Default.Decision != event.DecisionDeny {
		t.Fatalf("question = %+v, want the hold with a denial as its default", q)
	}
	if f.lookup.CallCount() != 0 {
		t.Fatalf("lookup ran %d times in the held batch", f.lookup.CallCount())
	}
	resume := *res.Parked
	resume.Answers = map[string]event.Answer{resume.CallID: {QuestionID: q.ID, Decision: event.DecisionApprove}}
	_, res = f.run(t, native.Turn{Text: "go", Park: true, Holds: holds, Resume: &resume})
	if res.Parked != nil || res.Text != "Done." || f.lookup.CallCount() != 1 {
		t.Fatalf("Result = %+v with %d lookups, want the batch run once and the turn finished", res, f.lookup.CallCount())
	}

	g := newParkFixture(t, nil, batch, agenttest.TextResponse("Done."))
	if _, res := g.run(t, native.Turn{Text: "go", Holds: holds}); res.Parked != nil || g.lookup.CallCount() != 1 {
		t.Fatalf("a turn that may not park: %+v with %d lookups, want it run", res, g.lookup.CallCount())
	}
}
