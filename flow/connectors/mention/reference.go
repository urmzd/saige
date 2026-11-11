package mention

import (
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Kinds of reference. They are the names event.Reference carries on the wire.
const (
	KindAgent  = "agent"
	KindPerson = "person"
	KindPlace  = "place"
	KindLink   = "link"
)

// Reference is one thing a text refers to: an agent or a person it names, a
// place it points at, or a link it carries.
//
// It mirrors event.Reference field for field, so a connector converts one
// with event.Reference(r). The type is repeated rather than imported because
// this package is a stdlib leaf, and that conversion stops compiling the
// moment the two disagree.
type Reference struct {
	// Kind is KindAgent, KindPerson, KindPlace or KindLink. A link is a URL
	// the text carried; a place is one it named without a URL ("#12",
	// "<#C02|general>").
	Kind string
	// Name is the agent's name for an agent, and otherwise what a person saw
	// in the text: "@ada", "#general", "#12", a link's label. Empty when they
	// saw only an id, or the URL itself.
	Name string
	// ID is the native id on the surface the text was written on: a Slack
	// user or channel id, a GitHub login, a Jira account id.
	ID string
	// Address is the canonical address of what is referred to, when it is
	// known: "github://acme/web/pull/12", "slack://T01/C02?thread=1699.001",
	// or what Links.Address returned for a URL, such as a session id.
	Address string
	// URL is the link exactly as it was written, line ranges and all.
	URL string
}

// Links is what a deployment knows about the URLs its people paste. The zero
// value knows only the forms ParseLink reads on its own.
type Links struct {
	// SlackTeams maps a Slack workspace's domain, the part of a permalink's
	// host before ".slack.com" in lower case ("acme" for acme.slack.com), to
	// its team id. A permalink on a domain it does not name keeps its URL and
	// gets no address: guessing the team would address another workspace.
	SlackTeams map[string]string
	// Address reads a URL none of the built-in forms recognise, such as a
	// link to one of the host's sessions, into its address. Nil reads none.
	Address func(*url.URL) (address string, ok bool)
}

// Scope is what the references in one text are read against: what the
// surface it was written on knows.
type Scope struct {
	// Agents decides which "@name" tokens name agents. Nil names none.
	Agents Set
	// People reads every other "@name" token as a person whose id is the
	// token. It is right where "@login" is how the surface writes a person,
	// as on GitHub, and wrong where people arrive as ids, as on Slack and
	// Jira, whose literal "@bob" names nobody.
	People bool
	// Self is the app's own handle: its mention names neither an agent nor a
	// person. Agents is asked first, so a handle mapped to an agent names it.
	Self Set
	// Repo is the GitHub repository a bare "#123" is read in, as
	// "owner/repo". Empty leaves "#123" alone; "owner/repo#123" needs no Repo.
	Repo string
	// Links is what the deployment knows about URLs.
	Links Links
}

// References returns what text refers to, in the order each is first
// written and once each (see Merge): "@name" tokens (Scope.Agents and
// Scope.People), GitHub
// issue and pull request references ("#12", "acme/web#12") and http or https
// URLs, each with its canonical address when this package or Scope.Links
// recognises it.
//
// Nothing inside a code span or a fenced code block is a reference: code
// quotes text, it addresses nobody. URLs are read first, so an "@" or a "#"
// inside one belongs to the URL. An "@" that follows name material is no
// mention (see scan), which keeps an email address from reading as a person,
// and an "@org/team" mention names a team, which is not a person either.
func References(text string, scope Scope) []Reference {
	code := codeSpans(text)
	var found []located
	skip := slices.Clone(code)
	for _, l := range urlsIn(text, code) {
		skip = append(skip, l.span)
		if r, ok := ParseLink(text[l.start:l.end], l.label, scope.Links); ok {
			found = append(found, located{at: l.start, ref: r})
		}
	}
	for _, s := range scan(text) {
		if within(skip, s.start) || teamMention(text, s.end) {
			continue
		}
		token := text[s.start+1 : s.end]
		if scope.Agents != nil {
			if name, ok := scope.Agents.Lookup(token); ok {
				found = append(found, located{at: s.start, ref: Reference{Kind: KindAgent, Name: name, ID: token}})
				continue
			}
		}
		if !scope.People {
			continue
		}
		if scope.Self != nil {
			if _, ok := scope.Self.Lookup(token); ok {
				continue
			}
		}
		found = append(found, located{at: s.start, ref: Reference{Kind: KindPerson, Name: "@" + token, ID: token}})
	}
	found = append(found, shortRefs(text, skip, scope.Repo)...)

	sort.SliceStable(found, func(i, j int) bool { return found[i].at < found[j].at })
	var out []Reference
	for _, f := range found {
		out = Merge(out, f.ref)
	}
	return out
}

// Merge appends to refs each of more that refs does not already hold. Two
// references are the same when they refer to the same thing: an agent by
// name, a person by id, a place by address and a link by URL. The first
// spelling seen is the one kept.
func Merge(refs []Reference, more ...Reference) []Reference {
	for _, r := range more {
		if !slices.ContainsFunc(refs, r.same) {
			refs = append(refs, r)
		}
	}
	return refs
}

func (r Reference) same(o Reference) bool {
	return r.Kind == o.Kind && r.identity() == o.identity()
}

// identity is what makes two references of one kind the same reference.
// Names and ids compare without case: GitHub logins and agent names ignore
// it, and Slack and Jira ids never differ by it alone.
func (r Reference) identity() string {
	switch r.Kind {
	case KindAgent:
		return strings.ToLower(r.Name)
	case KindPerson:
		if r.ID != "" {
			return strings.ToLower(r.ID)
		}
		return strings.ToLower(r.Name)
	case KindPlace:
		if r.Address != "" {
			return r.Address
		}
		return r.ID + "\x00" + r.Name
	default:
		return r.URL + "\x00" + r.Address
	}
}

// ParseLink reads one URL into a link reference, with its canonical address
// when it has a form below or Links.Address recognises it. label is what a
// person saw in its place, "" when they saw the URL itself. ok is false for
// anything but an absolute http or https URL.
//
// The forms read without help:
//
//   - A GitHub pull request or issue, with any tail (".../pull/12/files",
//     ".../pull/12#discussion_r5", ".../issues/7#issuecomment-1"):
//     github://<owner>/<repo>/{pull|issues}/<n>.
//   - A Slack conversation or message permalink on a domain
//     Links.SlackTeams names: ".../archives/C02" is slack://<team>/C02, and
//     ".../archives/C02/p1699123456001200" is the thread the message is in,
//     slack://<team>/C02?thread=1699123456.001200, or its thread_ts when the
//     message is a reply.
//
// A GitHub file or commit link keeps its URL, line range included, and has
// no address: no address names a file or a commit.
func ParseLink(raw, label string, links Links) (Reference, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Reference{}, false
	}
	r := Reference{Kind: KindLink, URL: raw}
	if label = strings.TrimSpace(label); label != raw {
		r.Name = label
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "github.com" || host == "www.github.com":
		r.Address = gitHubLink(u)
	case strings.HasSuffix(host, ".slack.com"):
		r.Address = slackLink(u, strings.TrimSuffix(host, ".slack.com"), links.SlackTeams)
	}
	if r.Address == "" && links.Address != nil {
		c := *u
		if a, ok := links.Address(&c); ok {
			r.Address = a
		}
	}
	return r, true
}

// gitHubLink is the address of a GitHub pull request or issue URL, "" for
// any other GitHub URL.
func gitHubLink(u *url.URL) string {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 4 || (segs[2] != "pull" && segs[2] != "issues") {
		return ""
	}
	n, ok := positive(segs[3])
	if !ok || !validLogin(segs[0]) || !validRepo(segs[1]) {
		return ""
	}
	return gitHubAddress(segs[0], segs[1], segs[2], n)
}

// gitHubAddress spells github://<owner>/<repo>/<kind>/<n> the way
// address.Address does.
func gitHubAddress(owner, repo, kind string, n int) string {
	return "github://" + owner + "/" + url.PathEscape(repo) + "/" + kind + "/" + strconv.Itoa(n)
}

// slackLink is the address of a Slack conversation or message permalink on
// domain, "" when teams does not name the domain or the path is neither.
func slackLink(u *url.URL, domain string, teams map[string]string) string {
	team := teams[domain]
	if domain == "" || !slackID(team) {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || len(segs) > 3 || segs[0] != "archives" || !slackID(segs[1]) {
		return ""
	}
	at := "slack://" + team + "/" + segs[1]
	if len(segs) == 2 {
		return at
	}
	ts, ok := permalinkTS(segs[2])
	if !ok {
		return ""
	}
	if parent := u.Query().Get("thread_ts"); validTS(parent) {
		ts = parent
	}
	return at + "?thread=" + url.QueryEscape(ts)
}

// permalinkTS reads the message timestamp out of a permalink's last segment:
// "p1699123456001200" is "1699123456.001200".
func permalinkTS(seg string) (string, bool) {
	digits, ok := strings.CutPrefix(seg, "p")
	if !ok || len(digits) <= 6 || !allDigits(digits) {
		return "", false
	}
	return digits[:len(digits)-6] + "." + digits[len(digits)-6:], true
}

func validTS(ts string) bool {
	secs, frac, ok := strings.Cut(ts, ".")
	return ok && allDigits(secs) && allDigits(frac)
}

// slackID reports whether s looks like a Slack team, channel or user id.
func slackID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// located is a reference and where in the text it was written.
type located struct {
	at  int
	ref Reference
}

// urlSpan is one URL in a text and the label a person saw for it.
type urlSpan struct {
	span
	label string
}

// urlsIn finds the http and https URLs in text outside code.
//
// A URL starts at "http://" or "https://" not glued to a word, and runs to
// whitespace or a character no URL is written with. The punctuation a
// sentence puts after it, and a closing bracket it did not open, are not
// part of it: "(see https://x.com/a)." is https://x.com/a.
func urlsIn(text string, code []span) []urlSpan {
	var out []urlSpan
	for i := 0; i < len(text); {
		k := strings.Index(text[i:], "http")
		if k < 0 {
			break
		}
		start := i + k
		rest := text[start:]
		if !strings.HasPrefix(rest, "http://") && !strings.HasPrefix(rest, "https://") ||
			within(code, start) || (start > 0 && alnum(text[start-1])) {
			i = start + 4
			continue
		}
		end := start
		for end < len(text) && !endsURL(text[end]) {
			end++
		}
		end = trimURL(text, start, end)
		out = append(out, urlSpan{span: span{start: start, end: end}, label: markdownLabel(text, start)})
		i = max(end, start+4)
	}
	return out
}

func endsURL(c byte) bool {
	return c <= ' ' || strings.IndexByte("<>\"'`|", c) >= 0
}

// trimURL drops what a sentence put after a URL: trailing punctuation, and a
// closing bracket the URL did not open.
func trimURL(text string, start, end int) int {
	for end > start {
		u := text[start:end]
		switch c := u[len(u)-1]; {
		case strings.IndexByte(".,;:!?*_~", c) >= 0:
			end--
		case c == ')' && strings.Count(u, "(") < strings.Count(u, ")"),
			c == ']' && strings.Count(u, "[") < strings.Count(u, "]"):
			end--
		default:
			return end
		}
	}
	return end
}

// markdownLabel is the label of a Markdown link "[label](url)" whose URL
// starts at start, and "" when the URL is not one.
func markdownLabel(text string, start int) string {
	if start < 2 || text[start-2:start] != "](" {
		return ""
	}
	depth := 0
	for i := start - 3; i >= 0 && text[i] != '\n'; i-- {
		switch text[i] {
		case ']':
			depth++
		case '[':
			if depth == 0 {
				return strings.TrimSpace(text[i+1 : start-2])
			}
			depth--
		}
	}
	return ""
}

// shortRefs finds GitHub's "#12" and "owner/repo#12" outside skip. A bare
// "#12" is read in repo and is left alone when repo is empty. GitHub serves
// a pull request at its issue number too, so a short reference, which does
// not say which it is, is addressed as an issue.
func shortRefs(text string, skip []span, repo string) []located {
	var out []located
	for i := 0; i < len(text); i++ {
		if text[i] != '#' || within(skip, i) {
			continue
		}
		j := i + 1
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		if j == i+1 || (j < len(text) && nameRune(rune(text[j]))) {
			continue
		}
		n, ok := positive(text[i+1 : j])
		if !ok {
			continue
		}
		start, owner, name := i, "", ""
		switch {
		case i > 0 && repoByte(text[i-1]):
			// "owner/repo#12": the whole token is the reference.
			k := i
			for k > 0 && repoByte(text[k-1]) {
				k--
			}
			if k < 2 || text[k-1] != '/' {
				continue
			}
			m := k - 1
			for m > 0 && loginByte(text[m-1]) {
				m--
			}
			if m > 0 && !refBoundary(text[m-1]) {
				continue
			}
			start, owner, name = m, text[m:k-1], text[k:i]
		case i > 0 && !refBoundary(text[i-1]):
			continue
		default:
			var cut bool
			if owner, name, cut = strings.Cut(repo, "/"); !cut {
				continue
			}
		}
		if !validLogin(owner) || !validRepo(name) {
			continue
		}
		out = append(out, located{at: start, ref: Reference{
			Kind: KindPlace, Name: text[start:j], Address: gitHubAddress(owner, name, "issues", n),
		}})
		i = j - 1
	}
	return out
}

// refBoundary reports whether a short reference may follow c: not glued to
// a word, a path, a domain or an HTML entity ("&#123;").
func refBoundary(c byte) bool {
	return !nameRune(rune(c)) && strings.IndexByte("/.&#@`~", c) < 0
}

// teamMention reports whether the "@name" ending at end is an "@org/team".
func teamMention(text string, end int) bool {
	return end+1 < len(text) && text[end] == '/' && nameRune(rune(text[end+1]))
}

// validLogin reports whether s is usable as a GitHub account name: letters,
// digits and hyphens, not starting with a hyphen, at most 39 long.
func validLogin(s string) bool {
	if s == "" || len(s) > 39 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !loginByte(s[i]) {
			return false
		}
	}
	return true
}

// validRepo reports whether s is usable as a GitHub repository name.
func validRepo(s string) bool {
	if s == "" || len(s) > 100 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !repoByte(s[i]) {
			return false
		}
	}
	return true
}

func loginByte(c byte) bool { return alnum(c) || c == '-' }

func repoByte(c byte) bool { return alnum(c) || c == '-' || c == '_' || c == '.' }

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// positive reads a positive decimal number.
func positive(s string) (int, bool) {
	if !allDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

// within reports whether offset i falls inside one of spans.
func within(spans []span, i int) bool {
	for _, s := range spans {
		if i >= s.start && i < s.end {
			return true
		}
	}
	return false
}

// codeSpans locates the code in text: fenced blocks, opened by a line of
// three or more backticks or tildes and closed by a line of at least as many
// of the same, and inline code, a run of backticks closed by a run of the
// same length. An unclosed fence runs to the end of the text; an unclosed
// backtick run is literal text.
func codeSpans(text string) []span {
	var out []span
	fence, fenceStart, prose := "", 0, 0
	for off := 0; off < len(text); {
		end, next := len(text), len(text)
		if nl := strings.IndexByte(text[off:], '\n'); nl >= 0 {
			end, next = off+nl, off+nl+1
		}
		line := strings.TrimRight(strings.TrimLeft(text[off:end], " "), " \t\r")
		switch {
		case fence == "":
			if f := fenceOf(line); f != "" {
				out = append(out, inlineCode(text, prose, off)...)
				fence, fenceStart = f, off
			}
		case strings.HasPrefix(line, fence) && strings.Trim(line, fence[:1]) == "":
			out = append(out, span{start: fenceStart, end: next})
			fence, prose = "", next
		}
		off = next
	}
	if fence != "" {
		return append(out, span{start: fenceStart, end: len(text)})
	}
	return append(out, inlineCode(text, prose, len(text))...)
}

// fenceOf is the fence a line opens, "" when it opens none.
func fenceOf(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	return line[:n]
}

// inlineCode locates the inline code spans in text[from:to].
func inlineCode(text string, from, to int) []span {
	var out []span
	for i := from; i < to; {
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < to && text[j] == '`' {
			j++
		}
		closed := false
		for k := j; k < to; {
			if text[k] != '`' {
				k++
				continue
			}
			m := k
			for m < to && text[m] == '`' {
				m++
			}
			if m-k == j-i {
				out = append(out, span{start: i, end: m})
				i, closed = m, true
				break
			}
			k = m
		}
		if !closed {
			i = j
		}
	}
	return out
}
