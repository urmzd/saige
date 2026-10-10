package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
)

// iconFilter marks an active transcript filter in the header.
const iconFilter = "▽"

// filterKinds are the entry kinds a filter can name with kind:, in the
// order the help lists them.
var filterKinds = []string{
	"user", "text", "thinking", "tool", "media", "citation", "refusal",
	"approval", "usage", "notice", "error", "stopped",
}

// transcriptFilter narrows the transcript to entries of some kinds and
// containing some words. The query is space-separated terms: kind:NAME
// (or k:NAME; several names separated by commas) keeps entries of those
// kinds, and every other term must appear in the entry, ignoring case.
type transcriptFilter struct {
	raw     string
	kinds   []string
	words   []string
	unknown []string
}

func parseFilter(q string) transcriptFilter {
	f := transcriptFilter{raw: strings.TrimSpace(q)}
	for _, term := range strings.Fields(f.raw) {
		value, isKind := strings.CutPrefix(term, "kind:")
		if !isKind {
			value, isKind = strings.CutPrefix(term, "k:")
		}
		if !isKind {
			f.words = append(f.words, strings.ToLower(term))
			continue
		}
		for _, k := range strings.Split(strings.ToLower(value), ",") {
			switch {
			case k == "":
			case slices.Contains(filterKinds, k):
				f.kinds = append(f.kinds, k)
			default:
				f.unknown = append(f.unknown, k)
			}
		}
	}
	return f
}

// active reports whether the filter hides anything.
func (f transcriptFilter) active() bool { return f.raw != "" }

// match reports whether e is shown.
func (f transcriptFilter) match(e activityEntry) bool {
	if !f.active() {
		return true
	}
	if (len(f.kinds) > 0 || len(f.unknown) > 0) && !slices.Contains(f.kinds, entryKind(e)) {
		return false
	}
	if len(f.words) == 0 {
		return true
	}
	text := strings.ToLower(entrySearchText(e))
	for _, w := range f.words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// apply returns the entries f shows.
func (f transcriptFilter) apply(entries []activityEntry) []activityEntry {
	if !f.active() {
		return entries
	}
	out := make([]activityEntry, 0, len(entries))
	for _, e := range entries {
		if f.match(e) {
			out = append(out, e)
		}
	}
	return out
}

// badge is the header's filter indicator: the glyph, the query, and how
// many entries it shows.
func (f transcriptFilter) badge(shown, total int, editing bool) string {
	if !f.active() && !editing {
		return ""
	}
	q := f.raw
	if q == "" {
		q = "type to filter"
	}
	s := fmt.Sprintf("%s %s  %d/%d", iconFilter, q, shown, total)
	if len(f.unknown) > 0 {
		s += "  unknown kind " + strings.Join(f.unknown, ",")
	}
	return s
}

// entryKind is the filter name of an entry's kind.
func entryKind(e activityEntry) string {
	switch e.kind {
	case activityUser:
		return "user"
	case activityText:
		return "text"
	case activityThinking:
		return "thinking"
	case activityTool:
		return "tool"
	case activityMedia:
		return "media"
	case activityCitation:
		return "citation"
	case activityRefusal:
		return "refusal"
	case activityMarker:
		return "approval"
	case activityUsage:
		return "usage"
	case activityNotice:
		return "notice"
	case activityError:
		return "error"
	case activityStopped:
		return "stopped"
	}
	return ""
}

// entrySearchText is the text a filter's words are matched against.
func entrySearchText(e activityEntry) string {
	parts := []string{e.text, e.name, e.result, e.errMsg}
	if e.content != nil {
		parts = append(parts, e.content.String())
	}
	if e.args != nil {
		parts = append(parts, encodeArgs(e.args, false))
	}
	if e.media != nil {
		parts = append(parts, e.media.label())
	}
	for _, m := range e.toolMedia {
		parts = append(parts, m.label())
	}
	for _, m := range e.markers {
		parts = append(parts, m.Kind, m.Message)
	}
	return strings.Join(parts, "\n")
}

// newFilterInput returns the input a filter is typed in.
func newFilterInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = iconFilter + " "
	ti.Placeholder = "kind:tool,media words (Enter keeps, Esc clears)"
	return ti
}
