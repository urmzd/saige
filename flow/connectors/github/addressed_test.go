package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// addressed is a connector whose app comments as fiber[bot] and runs
// docs-bot.
func addressed(t *testing.T) (*github.Connector, <-chan event.Envelope) {
	t.Helper()
	return start(t, github.Config{Agents: mention.NewNames("docs-bot"), Login: "fiber[bot]"})
}

// comment is issueComment with its body replaced.
func comment(body string) string {
	return strings.Replace(issueComment,
		`"body": "can @docs-bot summarize what changed here, and @docs-bot check the retry path too?"`,
		`"body": `+quote(body), 1)
}

func quote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func serve(t *testing.T, c *github.Connector, event, delivery, body string) {
	t.Helper()
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, event, delivery, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func silent(t *testing.T, tags <-chan event.Envelope) {
	t.Helper()
	select {
	case e := <-tags:
		t.Errorf("published %s: %+v", e.Type, e.Data)
	default:
	}
}

// A comment that mentions the app and names no agent is addressed to the app,
// as Slack delivers a mention of its app. It is published naming no agent,
// without the app's mention in its text: the issue's members decide who
// answers.
func TestACommentToTheAppThatNamesNoAgentIsPublishedNamingNone(t *testing.T) {
	c, tags := addressed(t)
	serve(t, c, "issue_comment", "d-app", comment("@fiber what changed here?"))
	e := receive(t, tags)
	r := request(t, e)
	if len(r.Mentions) != 0 || r.Text != "what changed here?" {
		t.Fatalf("mentions %v with %q, want none with the app's mention taken out", r.Mentions, r.Text)
	}
	if e.Channel != "github://urmzd/fiber/issues/42" || r.Actor.ID != "urmzd" || e.Data.(event.GitHubIssueCommentCreated).CommentID != 2201234567 {
		t.Fatalf("event is %+v", e)
	}
}

// An agent the comment names is mentioned, wherever the app is mentioned,
// and the app's mention leaves the text as the agent's does.
func TestAnAgentNamedBesideTheAppIsMentioned(t *testing.T) {
	c, tags := addressed(t)
	for delivery, body := range map[string]string{
		"d-first": "@fiber @docs-bot summarize",
		"d-after": "@docs-bot summarize, @fiber",
	} {
		serve(t, c, "issue_comment", delivery, comment(body))
		r := request(t, receive(t, tags))
		if !slices.Equal(r.Mentions, []string{"docs-bot"}) || !strings.HasPrefix(r.Text, "summarize") || strings.Contains(r.Text, "@") {
			t.Fatalf("%q published %v with %q, want docs-bot without either mention", body, r.Mentions, r.Text)
		}
	}
}

// Every human event that reads text reads it the same way: a review, a
// review comment, a new issue and a new pull request addressed to the app are
// published.
func TestEveryHumanEventAddressedToTheAppIsPublished(t *testing.T) {
	pull := map[string]any{
		"number": 7, "title": "Coalesce edits", "body": "@fiber does this look right?", "state": "open",
		"user": map[string]string{"login": "urmzd", "type": "User"}, "head": map[string]string{"ref": "x", "sha": "deadbeef"},
	}
	repo := map[string]any{"name": "fiber", "full_name": "urmzd/fiber", "owner": map[string]string{"login": "urmzd", "type": "User"}}
	sender := map[string]string{"login": "urmzd", "type": "User"}
	cases := map[string]map[string]any{
		"pull_request_review": {
			"action": "submitted", "pull_request": pull, "repository": repo, "sender": sender,
			"review": map[string]any{"id": 3, "body": "@fiber does this look right?", "user": sender},
		},
		"pull_request_review_comment": {
			"action": "created", "pull_request": pull, "repository": repo, "sender": sender,
			"comment": map[string]any{"id": 4, "body": "@fiber does this look right?", "user": sender},
		},
		"issues": {
			"action": "opened", "repository": repo, "sender": sender,
			"issue": map[string]any{"number": 9, "title": "Retries", "body": "@fiber does this look right?", "state": "open", "user": sender},
		},
		"pull_request": {"action": "opened", "pull_request": pull, "repository": repo, "sender": sender},
	}
	c, tags := addressed(t)
	for name, body := range cases {
		raw, _ := json.Marshal(body)
		serve(t, c, name, "d-"+name, string(raw))
		r := request(t, receive(t, tags))
		if len(r.Mentions) != 0 || !strings.HasSuffix(r.Text, "does this look right?") || strings.Contains(r.Text, "@fiber") {
			t.Fatalf("%s published %v with %q", name, r.Mentions, r.Text)
		}
	}
}

// Nothing is published for a comment that addresses neither the app nor an
// agent, so a busy repository wakes nobody; nor by a connector that does not
// know its own login; nor for the app's own comments, however they mention
// it.
func TestACommentAddressedToNobodyPublishesNothing(t *testing.T) {
	c, tags := addressed(t)
	serve(t, c, "issue_comment", "d-nobody", comment("@urmzd can you take a look? fiber@example.com knows"))
	silent(t, tags)
	serve(t, c, "issue_comment", "d-bot", strings.Replace(comment("@fiber what changed?"),
		`"sender": {"login": "urmzd", "type": "User"}`, `"sender": {"login": "fiber[bot]", "type": "Bot"}`, 1))
	silent(t, tags)

	c, tags = start(t, github.Config{Agents: mention.NewNames("docs-bot")})
	serve(t, c, "issue_comment", "d-no-login", comment("@fiber what changed?"))
	silent(t, tags)
}

func TestALoginThatCannotBeUsedIsRefused(t *testing.T) {
	for name, cfg := range map[string]github.Config{
		"a login with a space": {Login: "fiber bot"},
		"a login with a slash": {Login: "acme/fiber"},
		"a bare suffix":        {Login: "[bot]"},
	} {
		cfg.API = github.NewFake()
		if _, err := github.New(cfg); err == nil {
			t.Errorf("%s: New accepted %+v", name, cfg)
		}
	}
}

// A login in Bots names its agent wherever it is mentioned, the app's own
// too. So when Bots maps the app's login, its mention names that agent, in
// the order the text names agents.
func TestTheAppsLoginInBotsMentionsItsAgent(t *testing.T) {
	c, tags := start(t, github.Config{
		Agents: mention.NewNames("docs-bot"), Bots: map[string]string{"fiber[bot]": "ops"},
		Login: "fiber[bot]",
	})
	for delivery, body := range map[string]string{
		"d-alone":  "@fiber what changed?",
		"d-before": "@fiber @docs-bot summarize",
	} {
		serve(t, c, "issue_comment", delivery, comment(body))
		if r := request(t, receive(t, tags)); len(r.Mentions) == 0 || r.Mentions[0] != "ops" || strings.Contains(r.Text, "@fiber") {
			t.Fatalf("%q published %v with %q, want ops first without the app's mention", body, r.Mentions, r.Text)
		}
	}
}
