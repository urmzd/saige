package slack_test

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/event"
)

// withText is appMention with its text replaced by raw wire text.
func withText(raw string) string {
	return strings.Replace(appMention,
		`"text": "hey <@U08BOTID> can you summarize <https://example.com/design|the design doc> for <@U0HUMAN2>? &lt;3"`,
		`"text": `+jsonString(raw), 1)
}

// What Slack's markup refers to survives decoding as references, while the
// text stays what a human saw.
func TestSlackMarkupIsReadIntoReferences(t *testing.T) {
	const pr = "https://github.com/acme/web/pull/12/files#diff-abc"
	tests := []struct {
		name  string
		links mention.Links
		raw   string
		text  string
		want  []event.Reference
	}{
		{
			name: "a bot is an agent and a person keeps the id and the label",
			raw:  "<@U08BOTID> ask <@U0HUMAN2|ada> and <@U0HUMAN3>",
			text: "ask @ada and @U0HUMAN3",
			want: []event.Reference{
				{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "U08BOTID"},
				{Kind: event.ReferencePerson, Name: "@ada", ID: "U0HUMAN2"},
				{Kind: event.ReferencePerson, ID: "U0HUMAN3"},
			},
		},
		{
			name: "a channel is a place addressed in the team",
			raw:  "<@U08BOTID> post in <#C03OTHER|general> and <#C04BARE|>",
			text: "post in #general and #C04BARE",
			want: []event.Reference{
				{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "U08BOTID"},
				{Kind: event.ReferencePlace, Name: "#general", ID: "C03OTHER", Address: "slack://T01ABCDEF/C03OTHER"},
				{Kind: event.ReferencePlace, ID: "C04BARE", Address: "slack://T01ABCDEF/C04BARE"},
			},
		},
		{
			name: "a link keeps its URL and label, and a pull request its address",
			raw:  "<@U08BOTID> compare <https://example.com/design|the design doc> with <" + pr + ">",
			text: "compare the design doc with " + pr,
			want: []event.Reference{
				{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "U08BOTID"},
				{Kind: event.ReferenceLink, Name: "the design doc", URL: "https://example.com/design"},
				{Kind: event.ReferenceLink, URL: pr, Address: "github://acme/web/pull/12"},
			},
		},
		{
			name:  "a permalink on a known domain is its thread",
			links: mention.Links{SlackTeams: map[string]string{"acme": "T01ABCDEF"}},
			raw:   "<@U08BOTID> see <https://acme.slack.com/archives/C05X/p1700000000000200?thread_ts=1699999999.000100&amp;cid=C05X>",
			text:  "see https://acme.slack.com/archives/C05X/p1700000000000200?thread_ts=1699999999.000100&cid=C05X",
			want: []event.Reference{
				{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "U08BOTID"},
				{Kind: event.ReferenceLink, URL: "https://acme.slack.com/archives/C05X/p1700000000000200?thread_ts=1699999999.000100&cid=C05X",
					Address: "slack://T01ABCDEF/C05X?thread=1699999999.000100"},
			},
		},
		{
			name: "a permalink on an unknown domain keeps only its URL",
			raw:  "<@U08BOTID> see <https://acme.slack.com/archives/C05X/p1700000000000200>",
			text: "see https://acme.slack.com/archives/C05X/p1700000000000200",
			want: []event.Reference{
				{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "U08BOTID"},
				{Kind: event.ReferenceLink, URL: "https://acme.slack.com/archives/C05X/p1700000000000200"},
			},
		},
		{
			name: "broadcasts and mail links refer to nobody, and repeats are read once",
			raw:  "<@U08BOTID> <!here> mail <mailto:ada@example.com|ada@example.com>, <@U0HUMAN2|ada> <@U0HUMAN2|ada>",
			text: "@here mail ada@example.com, @ada @ada",
			want: []event.Reference{
				{Kind: event.ReferenceAgent, Name: "docs-bot", ID: "U08BOTID"},
				{Kind: event.ReferencePerson, Name: "@ada", ID: "U0HUMAN2"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}, Links: tc.links})
			c.ServeHTTP(httptest.NewRecorder(), post(t, withText(tc.raw)))
			b := posted(t, receive(t, tags))
			if b.Text != tc.text {
				t.Errorf("Text = %q, want %q", b.Text, tc.text)
			}
			if !reflect.DeepEqual(b.References, tc.want) {
				t.Errorf("References =\n%+v\nwant\n%+v", b.References, tc.want)
			}
		})
	}
}

// The app's own bot user, which every app_mention tags, is the addressee and
// not a person the message refers to, even when it fronts no agent.
func TestTheAppsOwnBotUserIsNoReference(t *testing.T) {
	c, tags := start(t, slack.Config{})
	c.ServeHTTP(httptest.NewRecorder(), post(t, withText("<@U08BOTID> ask <@U0HUMAN2|ada>")))
	b := posted(t, receive(t, tags))
	want := []event.Reference{{Kind: event.ReferencePerson, Name: "@ada", ID: "U0HUMAN2"}}
	if !reflect.DeepEqual(b.References, want) || len(b.Mentions) != 0 {
		t.Errorf("References = %+v, Mentions = %v, want only ada and no agent", b.References, b.Mentions)
	}
}

func TestASlashCommandCarriesItsReferences(t *testing.T) {
	c, tags := start(t, slack.Config{})
	c.ServeHTTP(httptest.NewRecorder(), command(t, invocation(map[string]string{"text": "for <@U0HUMAN2|ada> in <#C03OTHER|general>"})))
	b := receive(t, tags).Data.(event.SlackCommandInvoked)
	want := []event.Reference{
		{Kind: event.ReferencePerson, Name: "@ada", ID: "U0HUMAN2"},
		{Kind: event.ReferencePlace, Name: "#general", ID: "C03OTHER", Address: "slack://T01ABCDEF/C03OTHER"},
	}
	if !reflect.DeepEqual(b.References, want) || b.Text != "/create-pr for @ada in #general" {
		t.Errorf("References = %+v, Text = %q", b.References, b.Text)
	}
}
