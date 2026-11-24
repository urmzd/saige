package github_test

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// withIDs gives the sender and the issue or pull request's author numeric IDs.
func withIDs(body string, sender, author int64) string {
	body = strings.Replace(body, `"sender": {"login": "urmzd", "type": "User"}`, `"sender": {"login": "urmzd", "id": `+itoa(sender)+`, "type": "User"}`, 1)
	return strings.Replace(body, `"user": {"login": "urmzd", "type": "User"}`, `"user": {"login": "urmzd", "id": `+itoa(author)+`, "type": "User"}`, 1)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// reviewComment is a pull_request_review_comment delivery that names an agent.
const reviewComment = `{
  "action": "created",
  "pull_request": {
    "number": 77, "title": "Coalesce sink edits", "body": "", "state": "open",
    "user": {"login": "urmzd", "type": "User"},
    "html_url": "https://github.com/urmzd/fiber/pull/77",
    "base": {"ref": "main"}, "head": {"ref": "coalesce-edits", "sha": "deadbeef"}
  },
  "comment": {"id": 9, "body": "@docs-bot why?", "user": {"login": "urmzd", "type": "User"}, "created_at": "2026-07-21T10:00:00Z"},
  "repository": {"name": "fiber", "full_name": "urmzd/fiber", "owner": {"login": "urmzd", "type": "User"}},
  "sender": {"login": "urmzd", "type": "User"}
}`

// Every event that can start a run carries the numeric IDs of its sender and
// of the issue or pull request's author, and a review request also the
// requested reviewer's, beside the logins, on the body and in the trigger
// context.
func TestEventsCarryNumericUserIDs(t *testing.T) {
	opened := strings.Replace(strings.Replace(reviewRequested, `"action": "review_requested"`, `"action": "opened"`, 1), "cc @urmzd", "cc @docs-bot", 1)
	sync := strings.Replace(reviewRequested, `"action": "review_requested"`, `"action": "synchronize"`, 1)
	sync = strings.Replace(sync, `"sha": "deadbeef"`, `"sha": "`+commitSHA+`"`, 1)
	requested := strings.Replace(reviewRequested, `"requested_reviewer": {"login": "docs-bot[bot]", "type": "Bot"}`, `"requested_reviewer": {"login": "docs-bot[bot]", "id": 303, "type": "Bot"}`, 1)
	for _, tc := range []struct {
		name, event, body string
		reviewer          bool
	}{
		{"issue comment", "issue_comment", issueComment, false},
		{"review comment", "pull_request_review_comment", reviewComment, false},
		{"pull request opened", "pull_request", opened, false},
		{"pull request synchronize", "pull_request", sync, false},
		{"review requested", "pull_request", requested, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, tags := start(t, github.Config{
				Agents:     mention.NewNames("docs-bot"),
				Bots:       map[string]string{"docs-bot": "docs-bot"},
				Automation: func(github.Hook) (string, bool) { return "review the new commit", true },
			})
			c.ServeHTTP(httptest.NewRecorder(), deliver(t, tc.event, "d-"+tc.name, withIDs(tc.body, 101, 202)))
			e := receive(t, tags)
			got := ref(t, e)
			if got.Sender != "urmzd" || got.SenderID != 101 || got.Author != "urmzd" || got.AuthorID != 202 {
				t.Fatalf("ref %+v, want sender and author with their IDs", got)
			}
			if tc.reviewer && (got.RequestedReviewer != "docs-bot[bot]" || got.RequestedReviewerID != 303) {
				t.Fatalf("ref %+v, want the requested reviewer and its ID", got)
			}
			rendered, err := event.Render(e)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{"github_sender_id: 101", "github_author_id: 202"} {
				if !strings.Contains(rendered, line) {
					t.Errorf("trigger context lacks %q:\n%s", line, rendered)
				}
			}
			if tc.reviewer && !strings.Contains(rendered, "github_requested_reviewer_id: 303") {
				t.Errorf("trigger context lacks the requested reviewer's ID:\n%s", rendered)
			}
		})
	}
}

// A payload without IDs leaves them zero and out of the trigger context.
func TestEventsWithoutUserIDsLeaveThemZero(t *testing.T) {
	c, tags := start(t, github.Config{Agents: mention.NewNames("docs-bot")})
	c.ServeHTTP(httptest.NewRecorder(), deliver(t, "issue_comment", "d-none", issueComment))
	e := receive(t, tags)
	if got := ref(t, e); got.SenderID != 0 || got.AuthorID != 0 || got.RequestedReviewerID != 0 {
		t.Fatalf("ref %+v, want no IDs", got)
	}
	rendered, _ := event.Render(e)
	if strings.Contains(rendered, "_id: 0") || strings.Contains(rendered, "github_sender_id") {
		t.Fatalf("trigger context shows missing IDs:\n%s", rendered)
	}
}

// An ID below 1 in a payload is read as 0, the one value for unknown, and is
// left out of the trigger context.
func TestEventsReadUserIDsBelowOneAsZero(t *testing.T) {
	requested := strings.Replace(reviewRequested, `"requested_reviewer": {"login": "docs-bot[bot]", "type": "Bot"}`, `"requested_reviewer": {"login": "docs-bot[bot]", "id": -3, "type": "Bot"}`, 1)
	for _, tc := range []struct {
		name, event, body string
	}{
		{"issue comment", "issue_comment", issueComment},
		{"review requested", "pull_request", requested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, tags := start(t, github.Config{
				Agents: mention.NewNames("docs-bot"),
				Bots:   map[string]string{"docs-bot": "docs-bot"},
			})
			c.ServeHTTP(httptest.NewRecorder(), deliver(t, tc.event, "d-neg-"+tc.name, withIDs(tc.body, -7, -1)))
			e := receive(t, tags)
			if got := ref(t, e); got.SenderID != 0 || got.AuthorID != 0 || got.RequestedReviewerID != 0 {
				t.Fatalf("ref %+v, want every ID 0", got)
			}
			rendered, _ := event.Render(e)
			if strings.Contains(rendered, "_id: -") {
				t.Fatalf("trigger context shows a negative ID:\n%s", rendered)
			}
		})
	}
}
