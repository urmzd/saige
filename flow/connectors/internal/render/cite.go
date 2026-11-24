package render

import (
	"slices"
	"sort"
	"strings"
)

// Bounds on what a document keeps of a run's citations. A turn's retrieval
// and its tools decide how many sources it is given, so the document caps
// them rather than trusting the stream.
const (
	// MaxSources is the most distinct sources a document keeps. Citation
	// events for further sources are ignored.
	MaxSources = 256
	// MaxLabelsPerSource is the most labels a document keeps for one source.
	MaxLabelsPerSource = 32
	// maxLabelLen is the longest bracketed reference CitedLabels reads as
	// a citation; anything longer is prose in brackets, not a label.
	maxLabelLen = 64
)

// LabelKey returns the form of a citation label that an answer's reference
// is compared with: trimmed, without one pair of surrounding square
// brackets, and without a leading "^" (a Markdown footnote reference). The
// labels "[1]", "1" and "[^1]" share the key "1".
func LabelKey(label string) string {
	k := strings.TrimSpace(label)
	if len(k) >= 2 && k[0] == '[' && k[len(k)-1] == ']' {
		k = strings.TrimSpace(k[1 : len(k)-1])
	}
	return strings.TrimPrefix(k, "^")
}

// CitedLabels returns the keys (see LabelKey) of the labels text refers to.
//
// A reference is a bracketed group, such as "[2]", "[1, 3]", "[1][2]" or
// "[^2]", holding one or more labels separated by commas, semicolons or
// spaces. Brackets inside fenced code blocks and inline code spans are not
// references: there they are program text, such as an index. A label inside
// a quotation is a reference like any other.
func CitedLabels(text string) map[string]struct{} {
	out := make(map[string]struct{})
	prose := withoutCode(text)
	for i := 0; i < len(prose); i++ {
		if prose[i] != '[' {
			continue
		}
		end := strings.IndexAny(prose[i+1:], "[]\n")
		if end < 0 {
			break
		}
		if prose[i+1+end] != ']' || end > maxLabelLen {
			continue
		}
		inner := prose[i+1 : i+1+end]
		for _, tok := range strings.FieldsFunc(inner, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		}) {
			if k := LabelKey(tok); k != "" {
				out[k] = struct{}{}
			}
		}
		i += end
	}
	return out
}

// withoutCode returns text with its fenced code blocks and inline code spans
// replaced by spaces, so that what is left is the prose a reader sees.
func withoutCode(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	fence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if fence != "" {
			if closes(trimmed, fence) && indent <= 3 {
				fence = ""
			}
			blank(&b, line)
			continue
		}
		if f := fenceOf(trimmed); indent <= 3 && f != "" {
			fence = f
			blank(&b, line)
			continue
		}
		b.WriteString(withoutSpans(line))
	}
	return b.String()
}

// fenceOf returns the opening fence a line starts with: three or more
// backticks or tildes. It returns "" for any other line.
func fenceOf(line string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(line) && line[n] == c {
			n++
		}
		if n >= 3 {
			return line[:n]
		}
	}
	return ""
}

// closes reports whether line closes a block opened by fence: a run of the
// same character at least as long, and nothing else.
func closes(line, fence string) bool {
	line = strings.TrimRight(line, " \t\r\n")
	return len(line) >= len(fence) && strings.Trim(line, fence[:1]) == ""
}

// withoutSpans blanks the inline code spans of one line. A span opens with a
// run of backticks and closes at the next run of the same length; a run with
// no partner is a literal backtick.
func withoutSpans(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		n := run(line, i)
		closeAt := -1
		for j := i + n; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			m := run(line, j)
			if m == n {
				closeAt = j
				break
			}
			j += m
		}
		if closeAt < 0 {
			b.WriteString(line[i : i+n])
			i += n
			continue
		}
		blank(&b, line[i:closeAt+n])
		i = closeAt + n
	}
	return b.String()
}

// run returns the length of the run of backticks starting at i.
func run(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// blank writes s as spaces, keeping its line breaks.
func blank(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			b.WriteByte('\n')
		} else {
			b.WriteByte(' ')
		}
	}
}

// cited returns the sources text cites, in sequence order and numbered from
// one, each with Label set to the keys of the labels text cites it by.
func cited(cites []Citation, text string) []Citation {
	if len(cites) == 0 {
		return nil
	}
	refs := CitedLabels(text)
	if len(refs) == 0 {
		return nil
	}
	var out []Citation
	for _, c := range cites {
		var keys []string
		for _, l := range c.Labels {
			k := LabelKey(l)
			if _, ok := refs[k]; ok && !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			continue
		}
		c.Label = strings.Join(keys, ", ")
		c.Labels = append([]string(nil), c.Labels...)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	for i := range out {
		out[i].N = i + 1
	}
	return out
}
