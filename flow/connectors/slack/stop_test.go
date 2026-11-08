package slack_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/event"
)

// sessionStopped is the delivery Slack documents for a stop on a thread
// session. A code channel sends the same event without thread_ts.
const sessionStopped = `{
  "team_id": "T01ABCDEF",
  "api_app_id": "A08APPID",
  "event": {
    "type": "agent_session_stopped",
    "user": "U0HUMAN1",
    "channel": "C02CHANNEL",
    "thread_ts": "1700000000.000100",
    "streaming_message_ts": [],
    "event_ts": "1700000095.000300"
  },
  "type": "event_callback",
  "event_id": "Ev08STOPPED",
  "event_time": 1700000095
}`

const codeChannelStopped = `{
  "team_id": "T01ABCDEF",
  "event": {"type": "agent_session_stopped", "user": "U0HUMAN1", "channel": "C09CODE", "streaming_message_ts": []},
  "type": "event_callback",
  "event_id": "Ev08STOPCODE"
}`

func TestAStopOnASessionIsPublishedOnTheSessionChannel(t *testing.T) {
	cases := []struct {
		name, body, session, thread string
	}{
		{"a thread session", sessionStopped, "slack://T01ABCDEF/C02CHANNEL?thread=1700000000.000100", "1700000000.000100"},
		{"a code channel", codeChannelStopped, "slack://T01ABCDEF/C09CODE", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, tags := start(t, slack.Config{Team: "T01ABCDEF", Tenant: "acme"})

			w := httptest.NewRecorder()
			c.ServeHTTP(w, post(t, tc.body))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			e := <-tags
			if err := e.Validate(); err != nil {
				t.Fatalf("invalid stop: %v", err)
			}
			want := event.SlackSessionStopped{Team: "T01ABCDEF", Channel: e.Subject, ThreadTS: tc.thread, User: "U0HUMAN1"}
			if e.Type != event.TypeSlackSessionStopped || e.Channel != tc.session || e.Source != tc.session || e.Tenant != "acme" || e.Data != want {
				t.Fatalf("stop = %+v, want %s on %s by U0HUMAN1", e, event.TypeSlackSessionStopped, tc.session)
			}
		})
	}
}

func TestAStopIsRefusedUnsignedAndIgnoredFromAnotherTeam(t *testing.T) {
	t.Run("unsigned", func(t *testing.T) {
		c, tags := start(t, slack.Config{})
		r := post(t, sessionStopped)
		r.Header.Set("X-Slack-Signature", "v0=00")

		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)

		if w.Code == http.StatusOK || len(tags) != 0 {
			t.Fatalf("status = %d, published %d: an unsigned stop must not be published", w.Code, len(tags))
		}
	})
	t.Run("another team", func(t *testing.T) {
		c, tags := start(t, slack.Config{Team: "T09OTHER"})

		w := httptest.NewRecorder()
		c.ServeHTTP(w, post(t, sessionStopped))

		if w.Code != http.StatusOK || len(tags) != 0 {
			t.Fatalf("status = %d, published %d: another team's stop is acknowledged and dropped", w.Code, len(tags))
		}
	})
}
