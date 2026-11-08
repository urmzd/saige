package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

func TestLifecycleReactionsReplaceAnnouncementsAndSurviveRestart(t *testing.T) {
	ctx := context.Background()
	api := slack.NewFake()
	ts, err := api.PostMessage(ctx, slack.Message{Workspace: "T01", Channel: "C01", Text: "Question"})
	if err != nil {
		t.Fatal(err)
	}
	store := deliverystate.NewMemory()
	cfg := slack.Config{API: api, Tenant: "tenant", Team: "T01", StatusReactions: true, DeliveryState: store}
	c, err := slack.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	target := address.MustParse("slack://T01/C01").WithParam("thread", ts)
	makeEvent := func(seq uint64, b event.Body) event.Envelope {
		return event.Envelope{Tenant: "tenant", Run: "run", Channel: target.String(), Type: b.Type(), Seq: seq, Data: b}
	}
	if err := c.Deliver(ctx, target, makeEvent(1, event.Started{})); err != nil {
		t.Fatal(err)
	}
	if len(api.Messages()) != 1 {
		t.Fatal("working produced an announcement")
	}
	c, err = slack.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := makeEvent(2, event.Completed{Text: "Hello <@U0123>. See <https://example.com|the source>."})
	if err := c.Deliver(ctx, target, done); err != nil {
		t.Fatal(err)
	}
	if err := c.Deliver(ctx, target, makeEvent(1, event.Started{})); err != nil {
		t.Fatal(err)
	}
	origin, _ := api.Message(ts)
	if len(origin.Reactions) != 1 || origin.Reactions[0] != "white_check_mark" {
		t.Fatalf("reactions=%v", origin.Reactions)
	}
	messages := api.Messages()
	if len(messages) != 2 || !strings.Contains(messages[1].Text, "<@U0123>") {
		t.Fatalf("messages=%+v", messages)
	}
}
func TestOnlySignedKnownAnswerReactionsBecomeFeedback(t *testing.T) {
	var feedback []event.Feedback
	stops := 0
	c, _ := start(t, slack.Config{Tenant: "tenant", Team: "T01", Feedback: connectors.FeedbackHandler{
		Resolve: func(_ context.Context, tenant string, target address.Address) (connectors.FeedbackTarget, error) {
			return connectors.FeedbackTarget{Found: target.Path[0] == "C01", Run: "r1", Finding: "f1"}, nil
		},
		Record: func(_ context.Context, tenant, id string, b event.Feedback) error {
			feedback = append(feedback, b)
			return nil
		},
		Stop: func(context.Context, string, string, event.Actor) error { stops++; return nil },
	}})
	payload := `{"type":"event_callback","team_id":"T01","event_id":"e1","event":{"type":"reaction_added","event_ts":"11.000001","user":"U01","reaction":"-1","item":{"type":"message","channel":"C01","ts":"10.0"}}}`
	for _, change := range []struct {
		body     string
		unsigned bool
	}{
		{payload, false}, {strings.Replace(payload, "C01", "OTHER", 1), false}, {strings.Replace(payload, "T01", "OTHER", 1), false}, {payload, true},
	} {
		req := post(t, change.body)
		if change.unsigned {
			req.Header.Set("X-Slack-Signature", "v0=00")
		}
		w := httptest.NewRecorder()
		c.ServeHTTP(w, req)
		if change.unsigned && w.Code == http.StatusOK {
			t.Fatal("unsigned feedback accepted")
		}
	}
	if len(feedback) != 1 || feedback[0].FindingID != "f1" || feedback[0].Actor.ID != "U01" || feedback[0].Rating != "down" {
		t.Fatalf("feedback=%+v", feedback)
	}
	req := post(t, strings.Replace(payload, `"-1"`, `"raised_hand"`, 1))
	c.ServeHTTP(httptest.NewRecorder(), req)
	if stops != 1 {
		t.Fatal("stop not recorded")
	}
}

func TestPlainThanksInKnownThreadOnlyAddsReaction(t *testing.T) {
	api := slack.NewFake()
	ts, err := api.PostMessage(t.Context(), slack.Message{Workspace: "T01", Channel: "C01", Text: "thanks"})
	if err != nil {
		t.Fatal(err)
	}
	c, tags := start(t, slack.Config{API: api, Tenant: "tenant", Team: "T01", StatusReactions: true, ContinueThread: func(team, channel, thread string) bool { return thread == "root" }})
	payload := `{"type":"event_callback","team_id":"T01","event_id":"thanks","event":{"type":"message","user":"U01","channel":"C01","thread_ts":"root","ts":"` + ts + `","text":"Thank you!"}}`
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, payload))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-tags:
		t.Fatal("thanks started a model turn")
	default:
	}
	msg, _ := api.Message(ts)
	if len(msg.Reactions) != 1 || msg.Reactions[0] != "heart" {
		t.Fatal(msg)
	}
}
