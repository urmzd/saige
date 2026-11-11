package mention_test

import (
	"testing"

	"github.com/urmzd/saige/flow/connectors/mention"
)

func TestFindReturnsTheFirstTokenTheSetAccepts(t *testing.T) {
	set := mention.NewNames("docs-bot", "triage")

	tests := []struct {
		name  string
		text  string
		agent string
		ok    bool
	}{
		{name: "leading mention", text: "@docs-bot summarize", agent: "docs-bot", ok: true},
		{name: "mid sentence mention", text: "please ask @docs-bot to summarize", agent: "docs-bot", ok: true},
		{name: "unknown name is not an agent", text: "@urmzd take a look", ok: false},
		{name: "first known name wins over a later one", text: "cc @urmzd @triage @docs-bot", agent: "triage", ok: true},
		{name: "case insensitive, canonical spelling returned", text: "@Docs-Bot hi", agent: "docs-bot", ok: true},
		{name: "no mention at all", text: "just a comment", ok: false},
		{name: "bare at sign is not a mention", text: "@ docs-bot", ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent, _, ok := mention.Find(tc.text, set)
			if ok != tc.ok {
				t.Fatalf("Find(%q) ok = %v, want %v", tc.text, ok, tc.ok)
			}
			if agent != tc.agent {
				t.Errorf("Find(%q) agent = %q, want %q", tc.text, agent, tc.agent)
			}
		})
	}
}

func TestStrippingRemovesEveryMentionOfTheTaggedAgentAndNoOther(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		agent string
		want  string
	}{
		{
			name:  "leading mention and its space",
			text:  "@docs-bot summarize this thread",
			agent: "docs-bot",
			want:  "summarize this thread",
		},
		{
			name:  "trailing colon used as address",
			text:  "@docs-bot: summarize this",
			agent: "docs-bot",
			want:  "summarize this",
		},
		{
			name:  "dash used as address",
			text:  "@docs-bot - summarize this",
			agent: "docs-bot",
			want:  "summarize this",
		},
		{
			name:  "mid sentence leaves one space",
			text:  "please ask @docs-bot to summarize",
			agent: "docs-bot",
			want:  "please ask to summarize",
		},
		{
			name:  "every occurrence goes",
			text:  "@docs-bot summarize, then @docs-bot post it and cc @docs-bot",
			agent: "docs-bot",
			want:  "summarize, then post it and cc",
		},
		{
			name:  "other mentions survive untouched",
			text:  "@docs-bot ask @urmzd and @triage about it",
			agent: "docs-bot",
			want:  "ask @urmzd and @triage about it",
		},
		{
			name:  "mention at end of text",
			text:  "who can help with this @docs-bot",
			agent: "docs-bot",
			want:  "who can help with this",
		},
		{
			name:  "punctuation around the mention is kept",
			text:  "(@docs-bot) please look",
			agent: "docs-bot",
			want:  "() please look",
		},
		{
			name:  "an email address is not a mention",
			text:  "@docs-bot mail urmzd@example.com about it",
			agent: "docs-bot",
			want:  "mail urmzd@example.com about it",
		},
		{
			name:  "blockquote structure is preserved",
			text:  "> the build broke\n\n@docs-bot why?",
			agent: "docs-bot",
			want:  "> the build broke\n\nwhy?",
		},
		{
			name:  "indentation and code fences are not reflowed",
			text:  "@docs-bot fix:\n\n```go\n\tif err != nil {\n\t\treturn err\n\t}\n```",
			agent: "docs-bot",
			want:  "fix:\n\n```go\n\tif err != nil {\n\t\treturn err\n\t}\n```",
		},
		{
			name:  "text with no mention is returned unchanged",
			text:  "-- signed, a human",
			agent: "docs-bot",
			want:  "-- signed, a human",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mention.Strip(tc.text, tc.agent); got != tc.want {
				t.Errorf("Strip(%q, %q) = %q, want %q", tc.text, tc.agent, got, tc.want)
			}
			// Find must strip exactly what Strip does, so that a connector
			// which resolves the agent from a field and one which resolves it
			// from the text hand the agent the same prompt.
			if agent, stripped, ok := mention.Find(tc.text, mention.NewNames(tc.agent)); ok {
				if agent != tc.agent || stripped != tc.want {
					t.Errorf("Find = (%q, %q), want (%q, %q)", agent, stripped, tc.agent, tc.want)
				}
			}
		})
	}
}

func TestAnyAcceptsValidNamesAndRejectsUnusableOnes(t *testing.T) {
	tests := []struct {
		token string
		ok    bool
	}{
		{token: "docs-bot", ok: true},
		{token: "triage_2", ok: true},
		{token: "A", ok: true},
		{token: "", ok: false},
		{token: "docs.bot", ok: false},
		{token: "docs bot", ok: false},
		{token: "docs:bot", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			name, ok := mention.Any.Lookup(tc.token)
			if ok != tc.ok {
				t.Fatalf("Any.Lookup(%q) ok = %v, want %v", tc.token, ok, tc.ok)
			}
			if ok && name != tc.token {
				t.Errorf("Any.Lookup(%q) name = %q, want the token back", tc.token, name)
			}
		})
	}
}

func TestNewNamesDropsNamesNoTopicCouldHold(t *testing.T) {
	set := mention.NewNames("docs-bot", "bad name", "bad:name", "")
	if _, ok := set.Lookup("docs-bot"); !ok {
		t.Error("valid name was dropped")
	}
	for _, bad := range []string{"bad name", "bad:name", ""} {
		if _, ok := set.Lookup(bad); ok {
			t.Errorf("unusable name %q was kept", bad)
		}
	}
}

func TestNilSetFallsBackToAny(t *testing.T) {
	agent, stripped, ok := mention.Find("@docs-bot ping", nil)
	if !ok || agent != "docs-bot" || stripped != "ping" {
		t.Fatalf("Find with nil set = (%q, %q, %v), want (docs-bot, ping, true)", agent, stripped, ok)
	}
}
