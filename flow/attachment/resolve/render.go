package resolve

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urmzd/saige/flow/attachment"
)

// The text forms below are what a model reads. They are fixed strings built
// from the reference and the content only, with no timestamps or counters, so
// the same file always renders to the same bytes.

// describe is the attribute list shared by every form: what the file was
// called, what it is and how big it is. Name and alt text are the uploader's
// words; they were cleaned to one line at intake and are quoted here.
//
// A file that was never stored has no size or sniffed type to report, so
// both are left out rather than printed as zero.
func describe(a attachment.Attachment, mediaType string) string {
	var b strings.Builder
	if a.Unread != "" {
		fmt.Fprintf(&b, "name=%q", a.Label())
		if a.Alt != "" {
			fmt.Fprintf(&b, " alt=%q", a.Alt)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "name=%q type=%q bytes=\"%d\"", a.Label(), mediaType, a.Size)
	if a.Alt != "" {
		fmt.Fprintf(&b, " alt=%q", a.Alt)
	}
	return b.String()
}

// noteText is the floor: a file was attached and could not be read.
func noteText(a attachment.Attachment, mediaType, reason string) string {
	return fmt.Sprintf("[Attachment %s was attached but could not be read: %s.]", describe(a, mediaType), reason)
}

// nativeHeader introduces media sent as itself, so the model can refer to
// the file by name.
func nativeHeader(a attachment.Attachment, mediaType string) string {
	return fmt.Sprintf("[Attachment %s follows.]", describe(a, mediaType))
}

// block wraps text in a delimited, labelled block. extra is appended to the
// opening tag, for example to say the text is a transcript.
//
// A closing tag inside the content is defused so a file cannot end its own
// block early and continue as if it were the conversation. That is the only
// change made to content.
func block(a attachment.Attachment, mediaType, extra, text string, limit int) (string, bool) {
	shown, truncated := truncate(text, limit)
	shown = strings.ReplaceAll(shown, "</attachment", `<\/attachment`)
	var b strings.Builder
	b.WriteString("<attachment ")
	b.WriteString(describe(a, mediaType))
	if extra != "" {
		b.WriteString(" " + extra)
	}
	b.WriteString(">\n")
	b.WriteString(shown)
	if !strings.HasSuffix(shown, "\n") {
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "[truncated: the first %d of %d bytes are shown]\n", len(shown), len(text))
	}
	b.WriteString("</attachment>")
	return b.String(), truncated
}

// truncate keeps the beginning of s, at most limit bytes. It cuts at the last
// line break in the final tenth of the window when there is one, so a log or
// a transcript ends on a whole line, and otherwise on a character boundary.
func truncate(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl >= cut-cut/10 && nl > 0 {
		cut = nl + 1
	}
	return s[:cut], true
}

// size renders a byte count for a note.
func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// clock renders an offset into a recording as m:ss or h:mm:ss.
func clock(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// reasonOf extracts the reason from a Decline error.
func reasonOf(err error) string {
	msg := err.Error()
	if errors.Is(err, ErrNotApplicable) {
		if i := strings.LastIndex(msg, ErrNotApplicable.Error()+": "); i >= 0 {
			return msg[i+len(ErrNotApplicable.Error())+2:]
		}
	}
	return "it is not supported"
}

// joinReasons folds the reasons each ingestor gave into one clause, dropping
// repeats and keeping order.
func joinReasons(reasons []string) string {
	seen := make(map[string]bool, len(reasons))
	var out []string
	for _, r := range reasons {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return strings.Join(out, "; ")
}
