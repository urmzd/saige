package mention_test

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/mention"
)

// github is the scope a comment on acme/web is read in: docs-bot is an
// agent, the app is mentioned as @humagen, and acme.slack.com is team T01.
func github() mention.Scope {
	return mention.Scope{
		Agents: mention.NewNames("docs-bot"),
		People: true,
		Self:   mention.NewNames("humagen"),
		Repo:   "acme/web",
		Links: mention.Links{
			SlackTeams: map[string]string{"acme": "T01"},
			Address: func(u *url.URL) (string, bool) {
				id, ok := strings.CutPrefix(u.Path, "/sessions/")
				return id, ok && u.Host == "platform.example.com"
			},
		},
	}
}

func person(login string) mention.Reference {
	return mention.Reference{Kind: mention.KindPerson, Name: "@" + login, ID: login}
}

func place(name, at string) mention.Reference {
	return mention.Reference{Kind: mention.KindPlace, Name: name, Address: at}
}

func link(raw, at string) mention.Reference {
	return mention.Reference{Kind: mention.KindLink, URL: raw, Address: at}
}

func TestEachConstructIsReadWithItsCanonicalAddress(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []mention.Reference
	}{
		{name: "a login that names no agent is a person", text: "cc @ada please", want: []mention.Reference{person("ada")}},
		{name: "an agent is an agent, with the login as written", text: "@Docs-Bot look",
			want: []mention.Reference{{Kind: mention.KindAgent, Name: "docs-bot", ID: "Docs-Bot"}}},
		{name: "the app's own handle is nobody", text: "@humagen what changed?"},
		{name: "a team mention is not a person", text: "@acme/reviewers please look"},
		{name: "an email address is not a person", text: "mail ada@example.com"},
		{name: "a bare short reference is read in the repository", text: "fixes #12.",
			want: []mention.Reference{place("#12", "github://acme/web/issues/12")}},
		{name: "a qualified short reference names its own repository", text: "see urmzd/orchagent#14",
			want: []mention.Reference{place("urmzd/orchagent#14", "github://urmzd/orchagent/issues/14")}},
		{name: "a short reference glued to a word is not one", text: "PR#12 and foo#3 and &#123; and a/b/c#4"},
		{name: "a pull request URL with a review tail addresses the pull request",
			text: "https://github.com/acme/web/pull/12/files#r987",
			want: []mention.Reference{link("https://github.com/acme/web/pull/12/files#r987", "github://acme/web/pull/12")}},
		{name: "a discussion anchor addresses the pull request",
			text: "https://github.com/acme/web/pull/12#discussion_r55",
			want: []mention.Reference{link("https://github.com/acme/web/pull/12#discussion_r55", "github://acme/web/pull/12")}},
		{name: "an issue URL addresses the issue",
			text: "https://github.com/acme/web/issues/7#issuecomment-1",
			want: []mention.Reference{link("https://github.com/acme/web/issues/7#issuecomment-1", "github://acme/web/issues/7")}},
		{name: "a file permalink keeps its line range and has no address",
			text: "https://github.com/acme/web/blob/4f2a9c1/src/main.go#L10-L20",
			want: []mention.Reference{link("https://github.com/acme/web/blob/4f2a9c1/src/main.go#L10-L20", "")}},
		{name: "a commit link has no address",
			text: "https://github.com/acme/web/commit/4f2a9c1",
			want: []mention.Reference{link("https://github.com/acme/web/commit/4f2a9c1", "")}},
		{name: "a Slack permalink on a known team addresses its thread",
			text: "https://acme.slack.com/archives/C02/p1699123456001200",
			want: []mention.Reference{link("https://acme.slack.com/archives/C02/p1699123456001200", "slack://T01/C02?thread=1699123456.001200")}},
		{name: "a permalink to a reply addresses the thread it is in",
			text: "https://acme.slack.com/archives/C02/p1699123456001200?thread_ts=1699123000.000100&cid=C02",
			want: []mention.Reference{link("https://acme.slack.com/archives/C02/p1699123456001200?thread_ts=1699123000.000100&cid=C02", "slack://T01/C02?thread=1699123000.000100")}},
		{name: "a Slack channel link addresses the channel",
			text: "https://acme.slack.com/archives/C02",
			want: []mention.Reference{link("https://acme.slack.com/archives/C02", "slack://T01/C02")}},
		{name: "a permalink on an unknown team keeps only its URL",
			text: "https://other.slack.com/archives/C02/p1699123456001200",
			want: []mention.Reference{link("https://other.slack.com/archives/C02/p1699123456001200", "")}},
		{name: "a host link is addressed by the host",
			text: "continue https://platform.example.com/sessions/s_0123abcd",
			want: []mention.Reference{link("https://platform.example.com/sessions/s_0123abcd", "s_0123abcd")}},
		{name: "a Markdown link keeps its label",
			text: "read [the design doc](https://example.com/design).",
			want: []mention.Reference{{Kind: mention.KindLink, Name: "the design doc", URL: "https://example.com/design"}}},
		{name: "punctuation and an unopened bracket end a URL",
			text: "(see https://example.com/a_(b)), then https://example.com/c?d=1.",
			want: []mention.Reference{link("https://example.com/a_(b)", ""), link("https://example.com/c?d=1", "")}},
		{name: "an @ or # inside a URL belongs to the URL",
			text: "https://medium.com/@ada/post#12",
			want: []mention.Reference{link("https://medium.com/@ada/post#12", "")}},
		{name: "a scheme glued to a word is not a URL", text: "xhttps://example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mention.References(tc.text, github()); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("References(%q) =\n%+v\nwant\n%+v", tc.text, got, tc.want)
			}
		})
	}
}

func TestNothingInCodeIsAReference(t *testing.T) {
	for _, text := range []string{
		"run `@ada #12 https://github.com/acme/web/pull/1` first",
		"run ``a ` @ada`` first",
		"```\n@ada fixed #12 in https://github.com/acme/web/pull/1\n```",
		"~~~go\n// @ada #12\n~~~",
		"text\n````\n@ada\n```\nstill code #12\n````",
		"```\nan unclosed fence hides @ada to the end #12",
	} {
		if got := mention.References(text, github()); len(got) != 0 {
			t.Errorf("References(%q) = %+v, want none", text, got)
		}
	}
	// Code ends where its fence or its backticks close.
	got := mention.References("```\n@grace\n```\n`@grace` then @ada, and a lone ` @lin", github())
	if want := []mention.Reference{person("ada"), person("lin")}; !reflect.DeepEqual(got, want) {
		t.Errorf("References = %+v, want %+v", got, want)
	}
}

func TestReferencesAreReadOnceInTheOrderFirstWritten(t *testing.T) {
	text := "https://github.com/acme/web/pull/3 @ada #12 @docs-bot @Ada acme/web#12 " +
		"[again](https://github.com/acme/web/pull/3) @docs-bot"
	want := []mention.Reference{
		link("https://github.com/acme/web/pull/3", "github://acme/web/pull/3"),
		person("ada"),
		place("#12", "github://acme/web/issues/12"),
		{Kind: mention.KindAgent, Name: "docs-bot", ID: "docs-bot"},
	}
	if got := mention.References(text, github()); !reflect.DeepEqual(got, want) {
		t.Errorf("References =\n%+v\nwant\n%+v", got, want)
	}
}

func TestPeopleAreReadOnlyWhereTheSurfaceWritesThemAsNames(t *testing.T) {
	scope := github()
	scope.People = false
	got := mention.References("@docs-bot ask @ada about #12", scope)
	want := []mention.Reference{
		{Kind: mention.KindAgent, Name: "docs-bot", ID: "docs-bot"},
		place("#12", "github://acme/web/issues/12"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("References = %+v, want %+v", got, want)
	}
	// A bare short reference needs a repository to be read in.
	if got := mention.References("fixes #12", mention.Scope{}); len(got) != 0 {
		t.Errorf("References with no repository = %+v, want none", got)
	}
}

// Every address the grammar writes is one pkg/address reads and writes back
// unchanged, so a host can route on it as on any channel.
func TestAddressesAreTheAddressPackagesSpelling(t *testing.T) {
	text := "#12 acme/web.site#3 https://github.com/acme/web/pull/9/files " +
		"https://acme.slack.com/archives/C02/p1699123456001200?thread_ts=1699123000.000100 https://acme.slack.com/archives/C02"
	refs := mention.References(text, github())
	if len(refs) != 5 {
		t.Fatalf("References = %+v, want five", refs)
	}
	for _, r := range refs {
		a, err := address.Parse(r.Address)
		if err != nil {
			t.Errorf("%s: %v", r.Address, err)
			continue
		}
		if a.String() != r.Address {
			t.Errorf("address %q reads back as %q", r.Address, a)
		}
	}
}

func TestParseLinkReadsOnlyWebURLs(t *testing.T) {
	for _, raw := range []string{"mailto:ada@example.com", "tel:+15555550100", "/relative/path", "https://", "ftp://x.example.com"} {
		if r, ok := mention.ParseLink(raw, "", mention.Links{}); ok {
			t.Errorf("ParseLink(%q) = %+v, want nothing", raw, r)
		}
	}
	r, ok := mention.ParseLink("https://github.com/acme/web/pull/12", "https://github.com/acme/web/pull/12", mention.Links{})
	if !ok || r.Name != "" || r.Address != "github://acme/web/pull/12" {
		t.Errorf("ParseLink with its own URL as label = %+v, %v, want no name", r, ok)
	}
}
