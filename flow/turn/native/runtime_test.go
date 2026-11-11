package native_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
	"github.com/urmzd/saige/flow/turn/runtimetest"
)

func pinned() turn.Pinned {
	return turn.Pinned{
		Name:    "docs-bot",
		Version: "3",
		Digest:  "sha256:abc",
		Spec: turn.Spec{
			Name:         "docs-bot",
			Provider:     native.ProviderOffline,
			Model:        "scripted",
			SystemPrompt: "You document things.",
		},
	}
}

func posted(text string, context map[string]string) event.Envelope {
	return event.Envelope{
		ID:      "e1",
		Source:  "fiber:test",
		Type:    event.TypeMessagePosted,
		Channel: "fiber:test",
		Time:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Data:    event.MessagePosted{Request: event.Request{Text: text, Context: context}},
	}
}

func offline(answer string) *native.Runtime {
	return &native.Runtime{Options: []native.Option{native.WithScript(agenttest.TextResponse(answer))}}
}

func TestNativeConforms(t *testing.T) {
	t.Parallel()
	runtimetest.Run(t, runtimetest.Harness{
		New:    func(*testing.T) turn.Runtime { return offline("the docs are updated") },
		Pinned: pinned(),
		Input:  turn.Input{Trigger: posted("update the docs", nil)},
	})
}

// The prompt is the trigger's catalog rendering, context and all.
func TestThePromptIsTheTriggersRendering(t *testing.T) {
	t.Parallel()
	trigger := posted("update the docs", map[string]string{"repo": "fiber", "_private": "never"})
	want, err := native.Render(trigger)
	if err != nil {
		t.Fatal(err)
	}
	if want != event.RequestText("update the docs", map[string]string{"repo": "fiber"}) {
		t.Fatalf("rendering = %q", want)
	}
	res, err := offline("done").Turn(t.Context(), pinned(), turn.Input{Trigger: trigger}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompt != want {
		t.Fatalf("Prompt = %q, want %q", res.Prompt, want)
	}
	if strings.Contains(res.Prompt, "never") {
		t.Fatal("a private context key reached the model")
	}
}

// A trigger the catalog cannot render is refused before any model call.
func TestAnUnrenderableTriggerIsRefused(t *testing.T) {
	t.Parallel()
	unknown := event.Envelope{Type: "acme.thing.v1", Data: event.Unknown{TypeName: "acme.thing.v1"}}
	mismatched := posted("hi", nil)
	mismatched.Type = event.TypeCronTick
	for name, trigger := range map[string]event.Envelope{"unknown": unknown, "mismatched": mismatched, "empty": {}} {
		_, err := offline("x").Turn(t.Context(), pinned(), turn.Input{Trigger: trigger}, nil)
		if !errors.Is(err, native.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// A definition with no version, or whose spec names another agent, does not
// run.
func TestABadPinIsRefused(t *testing.T) {
	t.Parallel()
	other := pinned()
	other.Spec.Name = "other"
	unpinned := pinned()
	unpinned.Version = ""
	for name, def := range map[string]turn.Pinned{"spec": other, "unpinned": unpinned} {
		_, err := offline("x").Turn(t.Context(), def, turn.Input{Trigger: posted("hi", nil)}, nil)
		if !errors.Is(err, native.ErrInvalid) && !errors.Is(err, turn.ErrInvalid) {
			t.Errorf("%s: err = %v, want an invalid pin", name, err)
		}
	}
}

// A parked turn survives the trip through turn.Parked.
func TestParkedRoundTrips(t *testing.T) {
	t.Parallel()
	p := native.Parked{Prompt: "p", CallID: "c1", Pending: []byte(`{"role":"assistant","parts":[]}`)}
	p.Question.ID = "q1"
	p.Question.Prompt = "May I?"
	tp, err := native.ParkedTo(p)
	if err != nil {
		t.Fatal(err)
	}
	if tp.Question.ID != "q1" || tp.Prompt != "p" {
		t.Fatalf("turn.Parked = %+v", tp)
	}
	tp.Answers = map[string]event.Answer{"q1": {QuestionID: "q1", Decision: event.DecisionApprove}}
	back, err := native.ParkedOf(tp)
	if err != nil {
		t.Fatal(err)
	}
	if back.CallID != "c1" || back.Answers["q1"].Decision != event.DecisionApprove {
		t.Fatalf("native.Parked = %+v", back)
	}
	if _, err := native.ParkedOf(turn.Parked{}); !errors.Is(err, native.ErrInvalid) {
		t.Fatalf("a parked turn with no state: err = %v", err)
	}
}
