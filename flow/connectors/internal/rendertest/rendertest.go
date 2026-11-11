// Package rendertest checks that a connector's events render through the
// catalog as their request text, and render the same bytes after a JSON round
// trip, which is what a replay reads back.
package rendertest

import (
	"encoding/json"
	"testing"

	"github.com/urmzd/saige/flow/event"
)

// Check renders e through the catalog and fails t unless the bytes equal
// event.RequestText of its prompt and survive a JSON round trip unchanged. It
// returns the event's type.
func Check(t testing.TB, e event.Envelope) string {
	t.Helper()
	got, err := event.Render(e)
	if err != nil {
		t.Fatalf("rendertest: render %s: %v", e.Type, err)
	}
	text, meta, ok := event.Prompt(e.Data)
	if !ok {
		t.Fatalf("rendertest: %s asks nothing", e.Type)
	}
	if want := event.RequestText(text, meta); got != want {
		t.Errorf("rendertest: %s renders\n%q\nwant\n%q", e.Type, got, want)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("rendertest: marshal %s: %v", e.ID, err)
	}
	var back event.Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("rendertest: decode %s: %v", e.ID, err)
	}
	again, err := event.Render(back)
	if err != nil {
		t.Fatalf("rendertest: render %s after a round trip: %v", e.Type, err)
	}
	if again != got {
		t.Errorf("rendertest: %s renders\n%q\nafter a round trip, want\n%q", e.Type, again, got)
	}
	return e.Type
}
