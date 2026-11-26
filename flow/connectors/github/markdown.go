package github

import "strings"

// markdown is GitHub Flavored Markdown, the surface's native alphabet — and the
// one the model is already writing in, which is why the answer itself passes
// through untouched and only what this package composes is normalized here.
type markdown struct{}

func (markdown) Bold(s string) string   { return "**" + s + "**" }
func (markdown) Italic(s string) string { return "_" + s + "_" }

// Code renders an inline span. A backtick inside would close the span early and
// leave the rest of the line as markup, so it is replaced: the alternative — a
// longer fence — has to be computed from the content and still fails on the
// pathological case.
func (markdown) Code(s string) string {
	if strings.Contains(s, "`") {
		s = strings.ReplaceAll(s, "`", "'")
	}
	return "`" + s + "`"
}

// Link renders [text](uri). Brackets in the label are escaped, since they
// would close it early. In
// the uri, whitespace, parentheses, angle brackets and backslashes are
// percent-encoded, since Markdown reads them as the end of the destination or
// as escapes.
//
// A label that is the uri itself is escaped as text (see escapeText); any
// other label is the caller's to escape.
func (markdown) Link(text, uri string) string {
	if text == "" || text == uri {
		text = escapeText(uri)
	}
	return "[" + escapeBrackets(text) + "](" + linkURI.Replace(uri) + ")"
}

// escapeBrackets backslash-escapes each "[" and "]" in s that is not already
// escaped, so a label cannot close its link early.
func escapeBrackets(s string) string {
	if !strings.ContainsAny(s, "[]") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteByte(c)
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '[', ']':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// linkURI encodes the characters of a uri that end or alter a Markdown link
// destination.
var linkURI = strings.NewReplacer(
	" ", "%20", "\t", "%09", "\n", "%0A", "\r", "%0D",
	"(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "\\", "%5C",
)

func (markdown) Item(s string) string { return "- " + s }

// escapeText makes s read as plain text in GitHub Markdown within a line,
// inline or as a link's label.
//
// The ASCII punctuation Markdown reads as inline syntax (emphasis, code
// spans, links and images, raw HTML, autolinks, strikethrough, table cells,
// entities, math) is backslash-escaped, and so is a backslash, so a trailing
// one cannot escape what follows. GitHub also turns plain text into links
// and references; a zero-width joiner is put where it breaks each pattern:
// after every "@" (mentions), "#" (issue references such as #12 and
// owner/repo#5) and ":" (bare URLs and :emoji: codes), between "www" and the
// "." after it, and after the "-" of "GH-".
func escapeText(s string) string {
	const zwj = "\u200d"
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i, r := range s {
		switch {
		case r == '@' || r == '#' || r == ':':
			b.WriteRune(r)
			b.WriteString(zwj)
		case r == '.' && i >= 3 && strings.EqualFold(s[i-3:i], "www"):
			b.WriteString(zwj)
			b.WriteRune(r)
		case r == '-' && i >= 2 && strings.EqualFold(s[i-2:i], "gh"):
			b.WriteRune(r)
			b.WriteString(zwj)
		case r < 0x80 && strings.ContainsRune(markdownPunct, r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// markdownPunct is the ASCII punctuation escapeText backslash-escapes.
const markdownPunct = "\\`*_{}[]()<>!|~&$"
