// Package mention finds the agents a human tagged, takes the address off the
// text the agent will read while keeping the rest of the message whole, and
// reads everything else the text refers to.
//
// Every trigger faces the same two questions ("who was tagged?" and "what is
// left once the tag is removed?") and every native surface encodes a mention
// differently: Slack sends <@U08BOT>, Jira sends [~accountid:5b10a2], GitHub
// sends the literal @login. The encodings are the connectors' problem: each one
// normalizes its native markup into @name tokens and then calls this package,
// which knows only the normalized grammar. That split is what keeps the
// stripping rules (mid-sentence mentions, repeated mentions, the trailing colon
// people type after a name) in one tested place instead of three.
//
// A mention may sit anywhere in a message, and the whole message is what the
// agent reads. Text that arrives as "@docs-bot summarize this" is prompted as
// "summarize this": the name in front is only the address. Text that arrives
// as "could @docs-bot take a look?" is prompted as written, because cutting
// the name out leaves "could take a look?", words the person never wrote. A
// connector still normalizes native markup first, so no raw <@U08BOT> reaches
// a prompt. A name quoted in code (`@docs-bot help`) calls nobody.
//
// # References
//
// Agents are one of four things a text refers to. People, places and links
// are the others, and References reads them all into one typed list, the
// same on every surface: a person by native id, a place or a link with its
// canonical address when one is known (github://acme/web/pull/12,
// slack://T01/C02?thread=1699.001), a link with its URL as written. A
// connector reads what only it can see (Slack's <@U07|ada> and <#C02|general>
// markup) and hands URLs to ParseLink, so a pasted pull request or Slack
// permalink is addressed the same way whichever surface it was pasted on.
// What a deployment knows beyond this package (which team a Slack domain is,
// which URLs are its own sessions) arrives as Links. Who a person is stays
// the host's question: a reference carries the id, never a directory entry.
//
// This package is a leaf: stdlib only.
package mention

import (
	"slices"
	"strings"
)

// Set decides whether a mention token names an agent and returns the canonical
// spelling for it.
//
// It is one method because the two questions are one lookup: a deployment that
// knows its agents answers both from the same map, and a deployment that does
// not (Any) answers both from the token's syntax. Returning the canonical name
// means "@Docs-Bot" and "@docs-bot" produce the same mention, and therefore
// the same topic.
type Set interface {
	Lookup(token string) (name string, ok bool)
}

// Names is a Set over the agents a deployment runs. Lookup is
// case-insensitive and returns the spelling the set was built with, which is
// the spelling that ends up in the topic.
type Names map[string]string

// NewNames returns a Set over the given agent names. Names that are not usable
// as an agent name are dropped rather than stored, so a typo in configuration
// cannot make an unaddressable agent look addressable.
func NewNames(names ...string) Names {
	set := make(Names, len(names))
	for _, name := range names {
		if !Valid(name) {
			continue
		}
		set[strings.ToLower(name)] = name
	}
	return set
}

// Lookup implements Set.
func (n Names) Lookup(token string) (string, bool) {
	name, ok := n[strings.ToLower(token)]
	return name, ok
}

// Any accepts every syntactically valid agent name.
//
// It is the default because a connector is deployed before a registry exists to
// ask, and because the alternative default — accept nothing — would make a
// misconfigured deployment silently ignore every mention. Accepting any valid
// name only ever produces a tag for an agent that does not exist, which the
// core rejects with a diagnosable error.
var Any Set = anySet{}

type anySet struct{}

func (anySet) Lookup(token string) (string, bool) {
	if !Valid(token) {
		return "", false
	}
	return token, true
}

// Valid reports whether name is usable as an agent name: letters, digits,
// "-", "_", non-empty. The constraint comes from runtime.CheckRun, which will
// not accept anything else in a run's name, so a mention that could never
// name a run is rejected here where the error is still attributable to the
// text a human typed.
func Valid(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !nameRune(r) {
			return false
		}
	}
	return true
}

func nameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z',
		r >= 'A' && r <= 'Z',
		r >= '0' && r <= '9',
		r == '-', r == '_':
		return true
	}
	return false
}

// Find returns the first agent mentioned in text that set accepts, along with
// text stripped of EVERY mention of that agent.
//
// Every mention of the winning agent goes, not just the first: people write
// "@docs-bot can you ask @docs-bot's owner", and leaving the second copy in
// would put the agent's own name back in its prompt. Mentions of OTHER names
// are left exactly as they are — they may be humans the agent is being asked to
// involve, and rewriting them would lose information the agent needs.
//
// ok is false when no token in text names an agent, which is the normal case
// for the majority of events a webhook delivers.
func Find(text string, set Set) (agent, stripped string, ok bool) {
	if set == nil {
		set = Any
	}
	spans := scan(text)
	for _, s := range spans {
		name, ok := set.Lookup(text[s.start+1 : s.end])
		if !ok {
			continue
		}
		return name, strip(text, spans, name), true
	}
	return "", text, false
}

// FindAll returns every agent mentioned in text that set accepts, in the
// order each is first mentioned, and text stripped of every mention of each.
// It names who the text addresses and chooses none of them.
func FindAll(text string, set Set) (agents []string, stripped string) {
	if set == nil {
		set = Any
	}
	for _, s := range scan(text) {
		name, ok := set.Lookup(text[s.start+1 : s.end])
		if ok && !slices.Contains(agents, name) {
			agents = append(agents, name)
		}
	}
	stripped = text
	for _, a := range agents {
		stripped = Strip(stripped, a)
	}
	return agents, stripped
}

// Strip removes every mention of agent from text without needing a Set. It is
// the half of Find a connector wants when the agent was named by something
// other than the text — a GitHub review request names the reviewer in a field,
// yet the body may still mention it.
func Strip(text, agent string) string {
	if agent == "" {
		return text
	}
	return strip(text, scan(text), agent)
}

// span locates one "@name" token, from the "@" to one past the name.
type span struct{ start, end int }

// scan finds every mention token in text.
//
// A token counts only at a mention boundary: the start of the text, or after a
// rune that is not part of a name. That single rule is what keeps
// "mail urmzd@example.com" from reading as a mention of "example" — the "@"
// there follows "urmzd", which is name material — while still catching mentions
// in "(@docs-bot)" and "cc:@docs-bot".
func scan(text string) []span {
	var spans []span
	code := codeSpans(text)
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		// A name inside code is quoted, not called: "run `@docs-bot help`"
		// shows a command and asks nobody.
		if within(code, i) {
			continue
		}
		if i > 0 && nameRune(rune(text[i-1])) {
			continue
		}
		j := i + 1
		for j < len(text) && nameRune(rune(text[j])) {
			j++
		}
		if j == i+1 {
			continue
		}
		spans = append(spans, span{start: i, end: j})
		i = j - 1
	}
	return spans
}

// strip takes out the mention of agent that addresses the text, and leaves
// every other one where the person wrote it.
//
// A mention that opens the text is a form of address: "@docs-bot summarize
// this" asks "summarize this", and the name in front adds nothing the agent
// needs. A mention anywhere else is part of a sentence. Cut out, "could
// @docs-bot take a look?" becomes "could take a look?" and "cc @a and @b, both
// of you review" becomes "cc and , both of you review": the agent is handed
// words the person never wrote. So the whole message is kept, and only the
// address goes. Who was mentioned is said beside the text, never by editing it.
//
// Nothing else is touched: no reflow, which would corrupt the code blocks,
// tables and indentation of a real GitHub comment.
func strip(text string, spans []span, agent string) string {
	// The address is the run of mentions that opens the text: "@fiber
	// @docs-bot summarize" addresses two, and each is taken out in turn.
	var b strings.Builder
	prev, lead := 0, true
	for _, s := range spans {
		if !lead || !addressGap(text[prev:s.start]) {
			break
		}
		if strings.EqualFold(text[s.start+1:s.end], agent) {
			// Drop it with the gap before it; what follows keeps its own.
			prev = s.end
			continue
		}
		if b.Len() == 0 {
			// The first name kept opens the address: the gap after a dropped
			// name before it, such as ", ", is not the person's text.
			b.WriteString(text[s.start:s.end])
		} else {
			b.WriteString(text[prev:s.end])
		}
		prev = s.end
	}
	if prev == 0 {
		return strings.TrimSpace(text)
	}
	rest := text[prev:]
	if strings.TrimSpace(b.String()) == "" {
		// Nobody else is addressed: the punctuation after the name goes too.
		return trimAddress(strings.TrimSpace(rest))
	}
	return strings.TrimSpace(b.String() + rest)
}

// addressGap reports what may sit between the mentions that open a text and
// still leave them one address: nothing, or spaces and the commas people put
// between names.
func addressGap(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isSpace(s[i]) && s[i] != ',' {
			return false
		}
	}
	return true
}

// trimAddress trims the punctuation left when a mention was used as a form of
// address: "@docs-bot: summarize" and "@docs-bot - summarize" both become
// "summarize".
//
// It runs only when the mention opened the text, because only then is the
// leading punctuation residue of the removed name. Applied unconditionally it
// would eat the ">" of a Markdown blockquote or the "-" of a list item that
// merely happened to contain a mention further along.
func trimAddress(s string) string {
	for len(s) > 0 {
		switch s[0] {
		case ':', ',', ';', '-':
			s = strings.TrimSpace(s[1:])
		default:
			return s
		}
	}
	return s
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}
