package runtime

import (
	"testing"

	"github.com/urmzd/saige/flow/event"
)

// The relay a delegation runs is the same at every level, so a call made two
// delegations down arrives on the origin's stream exactly once, two levels
// deep, and nothing else a sub-task streams arrives at all. The real sandbox
// refuses a sub-task's own delegation by policy today; this holds the relay to
// any depth regardless.
func TestASubTaskOfASubTaskIsRelayedTwoLevelsDeepOnce(t *testing.T) {
	var origin []event.Body
	toOrigin := func(c event.Body) error {
		origin = append(origin, c)
		return nil
	}

	// origin <- child's sink <- grandchild's sink
	childSink := make(chan event.Body, 4)
	stopChild := relay(childSink, toOrigin)
	grandchildSink := make(chan event.Body, 4)
	stopGrandchild := relay(grandchildSink, func(c event.Body) error {
		childSink <- c
		return nil
	})

	grandchildSink <- event.Text{Text: "Three fail."}
	grandchildSink <- event.UsageCall{AccountingID: "r/a1/helper.sub-1/assistant.sub-2/1", Agent: "assistant", PromptTokens: 30}
	stopGrandchild()
	stopChild()

	if len(origin) != 1 {
		t.Fatalf("the origin received %d bodies, want the one run.usage.v1", len(origin))
	}
	got, ok := origin[0].(event.UsageCall)
	if !ok {
		t.Fatalf("the origin received %T, want event.UsageCall", origin[0])
	}
	if got.Depth != 2 || got.AccountingID != "r/a1/helper.sub-1/assistant.sub-2/1" || got.PromptTokens != 30 {
		t.Fatalf("relayed call = %+v, want it two levels deep and otherwise unchanged", got)
	}
}
