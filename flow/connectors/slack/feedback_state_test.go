package slack_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

func TestReactionRemovalOwnsOnlyItsAdditionAcrossRestartAndReordering(t *testing.T) {
	store := deliverystate.NewMemory()
	owner, comment := "", ""
	adds, withdrawals := 0, 0
	seen := map[string]bool{}
	cfg := slack.Config{Tenant: "tenant", Team: "T01", DeliveryState: store, Feedback: connectors.FeedbackHandler{
		Resolve: func(context.Context, string, address.Address) (connectors.FeedbackTarget, error) {
			return connectors.FeedbackTarget{Found: true, Run: "run", Finding: "finding"}, nil
		},
		Record: func(_ context.Context, _ string, id string, b event.Feedback) error {
			if b.Removed {
				t.Fatal("removal bypassed source ownership")
			}
			if !seen[id] {
				owner, comment = id, b.Comment
				adds++
				seen[id] = true
			}
			return nil
		},
		Withdraw: func(_ context.Context, _ string, id, source string, b event.Feedback) error {
			if !b.Removed || source == "" || b.Actor.ID != "U01" {
				t.Fatalf("invalid withdrawal: %s %+v", source, b)
			}
			withdrawals++
			if owner == source {
				owner, comment = "", ""
			}
			return nil
		},
	}}
	send := func(id, stamp, action string) {
		t.Helper()
		// Each delivery uses a new replica while retaining durable connector state.
		c, _ := start(t, cfg)
		body := fmt.Sprintf(`{"type":"event_callback","team_id":"T01","event_id":%q,"event":{"type":%q,"event_ts":%q,"user":"U01","reaction":"+1","item":{"type":"message","channel":"C01","ts":"10.0"}}}`, id, action, stamp)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, post(t, body))
		if w.Code != 200 {
			t.Fatalf("delivery %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	send("add-1", "11.000001", "reaction_added")
	owner, comment = "console", "newer same-rating assessment"
	send("remove-1", "12.000001", "reaction_removed")
	if owner != "console" || comment != "newer same-rating assessment" {
		t.Fatal("old reaction erased newer console feedback")
	}
	send("add-2", "14.000001", "reaction_added")
	send("delayed-remove", "13.000001", "reaction_removed")
	send("add-1", "11.000001", "reaction_added")
	if owner != "add-2" || adds != 2 || withdrawals != 1 {
		t.Fatalf("stale delivery changed current feedback: %s %d %d", owner, adds, withdrawals)
	}
	send("remove-2", "15.000001", "reaction_removed")
	send("remove-2", "15.000001", "reaction_removed")
	if owner != "" || withdrawals != 2 {
		t.Fatal("current native assessment was not removed once")
	}
	send("early-remove", "17.000001", "reaction_removed")
	send("late-add", "16.000001", "reaction_added")
	send("unordered-add", "", "reaction_added")
	if owner != "" || adds != 2 {
		t.Fatal("late or unordered addition resurrected removed feedback")
	}
}
