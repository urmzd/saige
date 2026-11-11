package slack_test

import (
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/rendertest"
	"github.com/urmzd/saige/flow/connectors/slack"
	fevent "github.com/urmzd/saige/flow/event"
)

// A mention and a slash command render through the event catalog as their
// request text.
func TestRenderMatchesRequestText(t *testing.T) {
	c, tags := start(t, slack.Config{
		Team: "T01ABCDEF",
		Bots: map[string]string{"U08BOTID": "docs-bot"},
	})
	for _, tc := range []struct {
		name  string
		serve func()
		want  string
	}{
		{"mention", func() { c.ServeHTTP(httptest.NewRecorder(), post(t, appMention)) }, fevent.TypeMessagePosted},
		{"command", func() { c.ServeHTTP(httptest.NewRecorder(), command(t, invocation(nil))) }, fevent.TypeSlackCommandInvoked},
	} {
		tc.serve()
		if typ := rendertest.Check(t, receive(t, tags)); typ != tc.want {
			t.Errorf("%s: type = %s, want %s", tc.name, typ, tc.want)
		}
	}
}
