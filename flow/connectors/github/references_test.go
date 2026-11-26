package github_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// A comment's logins, short references and links become references: a login
// that names no agent is a person, the app's own is nobody, and the agents
// stay in Mentions exactly as before.
func TestACommentReadsPeoplePlacesAndLinksBesideItsAgents(t *testing.T) {
	c, tags := start(t, github.Config{
		Agents: mention.NewNames("docs-bot"),
		Login:  "fiber[bot]",
		Links:  mention.Links{SlackTeams: map[string]string{"acme": "T01ABCDEF"}},
	})
	body := "@fiber @docs-bot ask @ada about #7 and urmzd/orchagent#14, " +
		"lines https://github.com/urmzd/fiber/pull/9/files#r5 from " +
		"https://acme.slack.com/archives/C02/p1700000000000100 (not `@grace` or ada@example.com)"
	serve(t, c, "issue_comment", "d-refs", comment(body))
	r := request(t, receive(t, tags))

	if !slices.Equal(r.Mentions, []string{"docs-bot"}) {
		t.Errorf("Mentions = %v, want only the agent", r.Mentions)
	}
	want := []event.Reference{
		{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "docs-bot"},
		{Kind: event.ReferencePerson, Name: "@ada", ID: "ada"},
		{Kind: event.ReferencePlace, Name: "#7", Address: "github://urmzd/fiber/issues/7"},
		{Kind: event.ReferencePlace, Name: "urmzd/orchagent#14", Address: "github://urmzd/orchagent/issues/14"},
		{Kind: event.ReferenceLink, URL: "https://github.com/urmzd/fiber/pull/9/files#r5", Address: "github://urmzd/fiber/pull/9"},
		{Kind: event.ReferenceLink, URL: "https://acme.slack.com/archives/C02/p1700000000000100", Address: "slack://T01ABCDEF/C02?thread=1700000000.000100"},
	}
	if !reflect.DeepEqual(r.References, want) {
		t.Errorf("References =\n%+v\nwant\n%+v", r.References, want)
	}
	if r.Text != "ask @ada about #7 and urmzd/orchagent#14, lines https://github.com/urmzd/fiber/pull/9/files#r5 from https://acme.slack.com/archives/C02/p1700000000000100 (not `@grace` or ada@example.com)" {
		t.Errorf("Text = %q, want the comment without the agent and app mentions", r.Text)
	}
}

// A person's login is a person even when it is mapped to no agent and the
// connector runs none: only Bots and Agents decide what an agent is.
func TestALoginIsAPersonUnlessItNamesAnAgent(t *testing.T) {
	c, tags := start(t, github.Config{Bots: map[string]string{"docs-bot[bot]": "docs-bot"}})
	serve(t, c, "issue_comment", "d-bots", comment("@docs-bot pair with @Urmzd and @urmzd"))
	r := request(t, receive(t, tags))
	want := []event.Reference{
		{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "docs-bot"},
		{Kind: event.ReferencePerson, Name: "@Urmzd", ID: "Urmzd"},
	}
	if !reflect.DeepEqual(r.References, want) {
		t.Errorf("References = %+v, want %+v", r.References, want)
	}
}

// A review request carries no comment: the pull request's own description is
// what it refers to.
func TestAReviewRequestReadsThePullRequestsDescription(t *testing.T) {
	c, tags := start(t, github.Config{Bots: map[string]string{"docs-bot[bot]": "docs-bot"}})
	raw, _ := json.Marshal(map[string]any{
		"action": "review_requested",
		"pull_request": map[string]any{
			"number": 7, "title": "Coalesce edits", "body": "Closes #3, follows up on @ada's review.", "state": "open",
			"user": map[string]string{"login": "urmzd", "type": "User"}, "head": map[string]string{"ref": "x", "sha": "deadbeef"},
		},
		"requested_reviewer": map[string]string{"login": "docs-bot[bot]", "type": "Bot"},
		"repository":         map[string]any{"name": "fiber", "full_name": "urmzd/fiber", "owner": map[string]string{"login": "urmzd", "type": "User"}},
		"sender":             map[string]string{"login": "urmzd", "type": "User"},
	})
	serve(t, c, "pull_request", "d-review", string(raw))
	r := request(t, receive(t, tags))
	want := []event.Reference{
		{Kind: event.ReferencePlace, Name: "#3", Address: "github://urmzd/fiber/issues/3"},
		{Kind: event.ReferencePerson, Name: "@ada", ID: "ada"},
	}
	if !reflect.DeepEqual(r.References, want) {
		t.Errorf("References = %+v, want %+v", r.References, want)
	}
}

// A reference is refused when its repository still ends in ".git" after the
// clone suffix is taken off, or when a name is past GitHub's length.
func TestReadReferencesBoundRepositoryNames(t *testing.T) {
	owner, repo := strings.Repeat("a", 39), strings.Repeat("r", 100)
	for reference, want := range map[string]string{
		"acme/docs.git":                       "github://acme/docs",
		"acme/docs.git#7":                     "github://acme/docs/issues/7",
		"https://github.com/acme/docs.git":    "github://acme/docs",
		owner + "/" + repo + "#7":             "github://" + owner + "/" + repo + "/issues/7",
		owner + "/" + repo + ".git":           "github://" + owner + "/" + repo,
		"acme/docs.git.git":                   "",
		"acme/docs.git.git#7":                 "",
		"acme/docs.GIT":                       "",
		"acme/docs.Git.git":                   "",
		"github://acme/docs.git.git/issues/7": "",
		"https://github.com/acme/docs.gIt":    "",
		"acme/.git":                           "",
		owner + "a/docs#7":                    "",
		"acme/" + repo + "r#7":                "",
		"github://acme/" + repo + "r/pull/7":  "",
		"https://github.com/" + owner + "a/x": "",
	} {
		got, err := github.ParseReadReference(reference)
		if want == "" {
			if err == nil {
				t.Errorf("%q was read as %s", reference, got)
			}
			continue
		}
		if err != nil || got.String() != want {
			t.Errorf("%q: %s, %v; want %s", reference, got, err, want)
		}
	}
}
