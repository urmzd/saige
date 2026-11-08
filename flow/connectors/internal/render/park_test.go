package render_test

import (
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
)

// A parked run shows what it asked while it waits, and how it was answered
// once it resumes; a question from a run it delegated to is shown until the
// delegation returns.
func TestAParkedRunShowsItsQuestionThenItsResolution(t *testing.T) {
	d := render.New()
	d.Apply(ev(1, event.TypeAccepted, `{}`))
	d.Apply(ev(2, event.TypeStarted, `{"attempt":1}`))
	d.Apply(ev(3, event.TypeToolCall, `{"id":"c1","name":"deploy","phase":"requested"}`))
	d.Apply(ev(9, event.TypeParked, `{"question":{"id":"q1","kind":"approval","prompt":"Deploy to prod?","tool":{"name":"deploy","call_id":"c1","arguments":{"env":"prod"}},"asked_of":"oncall"}}`))

	v := d.View()
	if v.Status != render.StatusParked || v.Question == nil || v.Question.Prompt != "Deploy to prod?" || v.Question.Tool != "deploy" || v.Question.Args != `{"env":"prod"}` || v.Question.AskedOf != "oncall" {
		t.Fatalf("parked view = %+v question %+v", v, v.Question)
	}

	d.Apply(ev(20, event.TypeResumed, `{"attempt":2,"answer":{"question_id":"q1","decision":"approve","by":"ana"}}`))
	v = d.View()
	if v.Status != render.StatusRunning || v.Question != nil || v.Answered == nil || v.Answered.Summary != "approved" || v.Answered.By != "ana" {
		t.Fatalf("resumed view = %+v answered %+v", v, v.Answered)
	}
	d.Apply(ev(30, event.TypeCompleted, `{"text":"Deployed."}`))
	if v := d.View(); v.Answered == nil || v.Question != nil {
		t.Errorf("completed view lost the resolution: %+v", v)
	}

	child := render.New()
	child.Apply(ev(2, event.TypeStarted, `{}`))
	child.Apply(ev(3, event.TypeChildParked, `{"run_id":"r-child","agent":"ops","question":{"id":"q2","kind":"choice","prompt":"Which env?","options":["prod","staging"]}}`))
	if v := child.View(); v.Status != render.StatusRunning || v.Waiting == nil || v.Waiting.Agent != "ops" || len(v.Waiting.Options) != 2 {
		t.Fatalf("delegating view = %+v waiting %+v", v, v.Waiting)
	}
	child.Apply(ev(4, event.TypeToolDone, `{"id":"d1","name":"tag_ops","result":"done"}`))
	if v := child.View(); v.Waiting != nil {
		t.Errorf("the child's question outlived the delegation: %+v", v.Waiting)
	}
}

// An expired question reads as nobody having answered.
func TestAnExpiredQuestionReadsAsNobodyAnswered(t *testing.T) {
	d := render.New()
	d.Apply(ev(9, event.TypeParked, `{"question":{"id":"q1","kind":"choice","prompt":"Which?","options":["a","b"]}}`))
	d.Apply(ev(20, event.TypeResumed, `{"answer":{"question_id":"q1","choice":"a","expired":true}}`))
	if a := d.View().Answered; a == nil || !a.Expired || a.Summary != "a" || a.By != "" {
		t.Fatalf("answered = %+v", a)
	}
}
