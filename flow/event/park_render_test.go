package event_test

import (
	"testing"

	"github.com/urmzd/saige/flow/event"
)

// A park on several questions renders every one, and a resume every answer,
// in order. A park on one renders as it always did.
func TestAParkOnSeveralQuestionsRendersEach(t *testing.T) {
	one := event.Question{ID: "q1", Prompt: "Which env?", Options: []string{"prod", "staging"}}
	two := event.Question{ID: "q2", Prompt: "Why?"}
	for _, tc := range []struct {
		body event.Body
		want string
	}{
		{event.Parked{Reason: "waiting on a person", Question: &one},
			"run r1 parked: waiting on a person question: Which env? (options: prod, staging)"},
		{event.Parked{Reason: "waiting on a person", Question: &one, Questions: []event.Question{one, two}},
			"run r1 parked: waiting on a person question: Which env? (options: prod, staging) question: Why?"},
		{event.Resumed{Attempt: 2, Answer: &event.Answer{Choice: "prod"}},
			"run r1 resumed, attempt 2 answer: prod"},
		{event.Resumed{Attempt: 2, Answer: &event.Answer{Choice: "prod"}, Answers: []event.Answer{{Choice: "prod"}, {Text: "audit"}}},
			"run r1 resumed, attempt 2 answer: prod answer: audit"},
	} {
		e := sample(tc.body)
		e.Run = "r1"
		got, err := event.Render(e)
		if err != nil {
			t.Fatalf("Render(%s): %v", e.Type, err)
		}
		if got != tc.want {
			t.Errorf("Render(%+v) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
