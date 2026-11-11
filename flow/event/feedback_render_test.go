package event_test

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/event"
)

func TestCommitEvidenceRendersWithoutHumanRating(t *testing.T) {
	body := event.Feedback{RunID: "r", Actor: event.Actor{ID: "commit:sha", Bot: true}, Rating: "up", Reasons: []string{"suggestion_committed"}, Comment: "https://github.com/acme/repo/commit/sha"}
	entry, ok := event.Lookup(event.TypeFeedback)
	if !ok {
		t.Fatal("feedback catalog entry missing")
	}
	got, err := entry.Render(event.Envelope{Type: event.TypeFeedback, Data: body})
	if err != nil || !strings.Contains(got, "verified suggestion commit") || strings.Contains(got, "up") {
		t.Fatal(got, err)
	}
}
