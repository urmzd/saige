package github_test

import (
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/internal/rendertest"
	"github.com/urmzd/saige/flow/connectors/mention"
	fevent "github.com/urmzd/saige/flow/event"
)

const issueOpened = `{
  "action": "opened",
  "issue": {
    "number": 9, "title": "@docs-bot please triage this", "body": "The sink posts twice on retry.",
    "state": "open", "user": {"login": "urmzd", "type": "User"}, "labels": [{"name": "bug"}, {"name": "sink"}],
    "created_at": "2026-07-22T08:00:00Z", "updated_at": "2026-07-22T08:00:00Z"
  },
  "repository": {"name": "fiber", "full_name": "urmzd/fiber", "owner": {"login": "urmzd", "type": "User"}},
  "sender": {"login": "urmzd", "type": "User"},
  "installation": {"id": 4242}
}`

// Each GitHub trigger renders through the event catalog as its request text.
func TestRenderMatchesRequestText(t *testing.T) {
	c, tags := start(t, github.Config{
		Agents: mention.NewNames("docs-bot"),
		Bots:   map[string]string{"docs-bot": "docs-bot"},
	})
	for _, tc := range []struct {
		event, body, want string
	}{
		{"issue_comment", issueComment, fevent.TypeGitHubIssueCommentCreated},
		{"pull_request", reviewRequested, fevent.TypeGitHubReviewRequested},
		{"issues", issueOpened, fevent.TypeGitHubIssueOpened},
	} {
		c.ServeHTTP(httptest.NewRecorder(), deliver(t, tc.event, "d-"+tc.want, tc.body))
		if typ := rendertest.Check(t, receive(t, tags)); typ != tc.want {
			t.Errorf("%s: type = %s, want %s", tc.event, typ, tc.want)
		}
	}
}
