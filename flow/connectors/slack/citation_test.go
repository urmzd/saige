package slack_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/event"
)

// retrieved returns citation events for three passages, as a turn that
// retrieved them publishes them, starting at seq.
func retrieved(seq uint64) []event.Envelope {
	var out []event.Envelope
	for i, name := range []string{"alpha", "beta", "gamma"} {
		out = append(out, ev(seq+uint64(i), event.TypeCitation, fmt.Sprintf(
			`{"label":"[%d]","index":%d,"text":"passage text of %s","source":{"document_title":"%s guide","source_uri":"https://example.com/%s"}}`,
			i+1, i, name, name, name)))
	}
	return out
}

func deliverAll(t *testing.T, c *slack.Connector, events []event.Envelope) {
	t.Helper()
	for _, e := range events {
		if err := c.Deliver(context.Background(), target, e); err != nil {
			t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
		}
	}
}

func TestMessageListsOnlyTheSourcesTheAnswerCites(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   []string
	}{
		{"one of three", "Beta covers it [2].", []string{"[2] <https://example.com/beta|beta guide>"}},
		{"none", "No source was needed.", nil},
		{"label only in code", "Read `xs[2]` to get it.", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := slack.NewFake()
			c := sinkConnector(t, fake)
			events := append([]event.Envelope{ev(1, event.TypeAccepted, "")}, retrieved(2)...)
			events = append(events, textEvent(5, tc.answer), ev(6, event.TypeCompleted, ""))
			deliverAll(t, c, events)

			messages := fake.Messages()
			if len(messages) != 1 {
				t.Fatalf("slack holds %d messages, want 1", len(messages))
			}
			body := messages[0].Text
			if strings.Contains(body, "passage text") {
				t.Errorf("message quotes passage text:\n%s", body)
			}
			if got := strings.Count(body, "https://example.com/"); got != len(tc.want) {
				t.Errorf("message has %d footnote links, want %d:\n%s", got, len(tc.want), body)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("message lacks %q:\n%s", w, body)
				}
			}
		})
	}
}
