package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/event"
)

func TestVerifiedMessagesStayWithinTheConfiguredTeam(t *testing.T) {
	for _, kind := range []string{"app_mention", "message"} {
		for _, tc := range []struct {
			name, configured, delivered string
			accepted                    bool
		}{
			{"same team", "T01ABCDEF", "T01ABCDEF", true},
			{"other team", "T01ABCDEF", "T09OTHER", false},
			{"missing team", "T01ABCDEF", "", false},
			{"unbound connector", "", "T09OTHER", true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				prepared := false
				c, tags := start(t, slack.Config{
					Team: tc.configured,
					Prepare: func(_ context.Context, _ *event.Envelope) error {
						prepared = true
						return nil
					},
				})
				body := strings.ReplaceAll(appMention, "T01ABCDEF", tc.delivered)
				body = strings.Replace(body, `"type": "app_mention"`, `"type": "`+kind+`", "channel_type": "im"`, 1)
				w := httptest.NewRecorder()
				c.ServeHTTP(w, post(t, body))
				if w.Code != http.StatusOK || prepared != tc.accepted {
					t.Fatalf("status=%d prepared=%v, want 200/%v", w.Code, prepared, tc.accepted)
				}
				if tc.accepted {
					if b := posted(t, receive(t, tags)); b.Slack.Team != tc.delivered {
						t.Fatalf("team=%q, want %q", b.Slack.Team, tc.delivered)
					}
				} else {
					select {
					case e := <-tags:
						t.Fatalf("out-of-team delivery published %+v", e)
					default:
					}
				}
			})
		}
	}
}
