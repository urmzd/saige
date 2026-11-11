package sink_test

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
)

// A run waiting on a person reads as waiting, in words: the question, what
// it would run, and nothing that looks like work in progress. The resolution
// is shown once it resumes.
func TestAParkedRunReadsAsWaitingOnAPersonAndNotAsWorking(t *testing.T) {
	d := render.New()
	d.Apply(ev(2, event.TypeStarted, `{}`))
	d.Apply(ev(3, event.TypeToolCall, `{"id":"c1","name":"deploy","phase":"requested"}`))
	d.Apply(ev(9, event.TypeParked, `{"question":{"id":"q1","kind":"approval","prompt":"Deploy to prod?","tool":{"name":"deploy","arguments":{"env":"prod"}}}}`))

	got := layout(d.View())
	for _, want := range []string{"docs-bot (waiting on a person)", "waiting on a person: Deploy to prod?", `approve or deny deploy {"env":"prod"}`, "deploy waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("layout is missing %q:\n%s", want, got)
		}
	}
	for _, animated := range []string{"working", "running", "..."} {
		if strings.Contains(got, animated) {
			t.Errorf("a parked run reads as %q:\n%s", animated, got)
		}
	}

	d.Apply(ev(20, event.TypeResumed, `{"answer":{"question_id":"q1","decision":"deny","by":"ana"}}`))
	d.Apply(ev(30, event.TypeCompleted, `{"text":"Not deployed."}`))
	got = layout(d.View())
	if !strings.Contains(got, "answered: denied by ana") || strings.Contains(got, "Deploy to prod?") {
		t.Errorf("after the answer:\n%s", got)
	}
}

// A run waiting on a run it delegated to reads as waiting on that agent, not
// on a person.
func TestARunWaitingOnAChildReadsAsWaitingOnThatAgent(t *testing.T) {
	d := render.New()
	d.Apply(ev(9, event.TypeParked, `{"question":{"id":"q1","kind":"delegation","prompt":"deploy","tool":{"name":"tag_ops"},"child_run_id":"r-child"}}`))
	got := layout(d.View())
	if !strings.Contains(got, "docs-bot (waiting on ops)") || !strings.Contains(got, "waiting on ops: deploy (run r-child)") || strings.Contains(got, "a person") {
		t.Errorf("layout:\n%s", got)
	}
}
