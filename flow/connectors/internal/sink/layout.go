package sink

import (
	"strings"

	"github.com/urmzd/saige/flow/connectors/internal/render"
)

// Markup is a surface's alphabet: the handful of things every one of these
// surfaces can do, spelled its own way.
//
// The split between Markup and Layout is where the duplication belongs. What a
// run's message contains and in what order — who is answering, which tools ran,
// the answer, the footnotes, the outcome — is one decision, made once in Layout
// and tested once. How bold text is spelled is three decisions, because Slack
// mrkdwn, GitHub Markdown and Jira wiki markup genuinely disagree, and pretending
// otherwise is how a connector ends up posting asterisks at a human.
type Markup interface {
	Bold(s string) string
	Italic(s string) string
	// Code renders an inline code span.
	Code(s string) string
	// Link renders text pointing at uri. A surface that cannot make a link
	// returns something a human can still copy.
	Link(text, uri string) string
	// Item renders one line of a bulleted list, including its marker.
	Item(s string) string
}

// MapText returns a copy of v with fn applied to every field that will be
// rendered as prose.
//
// It is how a surface with an escaping rule applies it: Slack needs &, < and >
// escaped or a mrkdwn body containing them renders as broken markup, and Jira
// needs its own metacharacters neutralized. Doing it here, once, at the boundary
// between "text that came from a model or a tool" and "markup we are composing",
// is the only place it can be done consistently — Layout composes markup, so by
// the time it runs, escaping could no longer tell the two apart.
//
// URIs are deliberately not mapped. They are rendered inside a link construct
// that has its own rules, and prose escaping would corrupt them.
func MapText(v render.View, fn func(string) string) render.View {
	if fn == nil {
		return v
	}
	out := v
	out.Text = fn(v.Text)
	out.Thinking = fn(v.Thinking)
	out.Error = fn(v.Error)
	out.Reason = fn(v.Reason)

	if len(v.Tools) > 0 {
		out.Tools = make([]render.Tool, len(v.Tools))
		for i, t := range v.Tools {
			t.Name, t.Args, t.Result, t.Error = fn(t.Name), fn(t.Args), fn(t.Result), fn(t.Error)
			out.Tools[i] = t
		}
	}
	if len(v.Citations) > 0 {
		out.Citations = make([]render.Citation, len(v.Citations))
		for i, c := range v.Citations {
			c.Title, c.Snippet = fn(c.Title), fn(c.Snippet)
			out.Citations[i] = c
		}
	}
	if len(v.Actions) > 0 {
		out.Actions = make([]render.Action, len(v.Actions))
		for i, a := range v.Actions {
			a.Summary = fn(a.Summary)
			out.Actions[i] = a
		}
	}
	return out
}

// StatusLabel is the human word for a run's state. Words rather than icons: a
// message body is read in three different surfaces with three different emoji
// vocabularies, and "working" needs no legend.
func StatusLabel(s render.Status) string {
	switch s {
	case render.StatusAccepted:
		return "queued"
	case render.StatusRunning:
		return "working"
	case render.StatusParked:
		return "waiting on a person"
	case render.StatusDone:
		return "done"
	case render.StatusFailed:
		return "failed"
	case render.StatusStopped:
		return "stopped"
	default:
		return ""
	}
}

// Layout renders a run's document in m's markup.
//
// The shape, top to bottom:
//
//	docs-bot · working                     who is answering, and whether it still is
//	· search {"q":"flow"} -> 3 hits     what it did, so a slow answer is legible
//	the answer, streaming                  the answer itself
//	[1] Design doc                         footnotes, one per cited source
//	changed: commented on PR #42           what it changed on a real surface
//	failed: provider timeout               why there is no answer
//	waiting on a person: Deploy to prod?   what a parked run asked, and of whom
//	answered: approved by ana              how the question was resolved
//	stopped: Stopped by a user             why the answer is not finished
//
// Sections that are empty are omitted entirely rather than rendered as a
// heading with nothing under it, so the first delta of a run produces a
// one-line message instead of a form.
func Layout(m Markup, v render.View) string {
	var sections []string

	if header := layoutHeader(m, v); header != "" {
		sections = append(sections, header)
	}
	if len(v.Tools) > 0 {
		sections = append(sections, layoutTools(m, v))
	}
	if v.Thinking != "" && v.Text == "" {
		// Reasoning stands in for the answer only until the answer starts. Once
		// there is text, the thinking is noise in a channel; a consumer that
		// wants it can subscribe to delta.thinking on the bus.
		sections = append(sections, m.Italic(collapse(v.Thinking)))
	}
	if v.Text != "" {
		sections = append(sections, strings.TrimRight(v.Text, " \t\n"))
	}
	if len(v.Citations) > 0 {
		sections = append(sections, layoutCitations(m, v))
	}
	if len(v.Actions) > 0 {
		sections = append(sections, layoutActions(m, v))
	}
	if v.Error != "" {
		sections = append(sections, m.Bold("failed")+": "+v.Error)
	}
	if q := v.Question; q != nil && q.Kind == "delegation" {
		// Not a person: the run waits on a run it delegated to.
		line := m.Bold("waiting on "+strings.TrimPrefix(q.Tool, "tag_")) + ": " + truncate(collapse(q.Prompt), 400)
		if q.Child != "" {
			line += " " + m.Italic("(run "+q.Child+")")
		}
		sections = append(sections, line)
	} else if q != nil {
		sections = append(sections, layoutQuestion(m, "waiting on a person", q))
	} else if q := v.Waiting; q != nil {
		who := "a delegated run"
		if q.Agent != "" {
			who = q.Agent
		}
		sections = append(sections, layoutQuestion(m, who+" is waiting on a person", q))
	}
	if a := v.Answered; a != nil {
		line := m.Bold("answered") + ": " + truncate(collapse(a.Summary), 200)
		switch {
		case a.Expired:
			line += " " + m.Italic("(nobody answered in time)")
		case a.By != "":
			line += " " + m.Italic("by "+truncate(collapse(a.By), 80))
		}
		sections = append(sections, line)
	}
	if v.Status == render.StatusStopped {
		// Always said in the body, not only in the header: a stopped run's
		// text is an unfinished answer, and a reader who skips the header
		// must not take it for a finished one.
		line := m.Bold("stopped")
		if v.Reason != "" {
			line += ": " + truncate(collapse(v.Reason), 200)
		}
		sections = append(sections, line)
	}

	return strings.Join(sections, "\n\n")
}

// layoutQuestion renders what a run waits on a person for. It is plain
// words, never a spinner or an ellipsis: a run waiting on a person is not
// working, and a surface must not look as if it were. Answering happens on
// the host, so the question is shown, not made interactive.
func layoutQuestion(m Markup, title string, q *render.Question) string {
	lines := []string{m.Bold(title) + ": " + truncate(collapse(q.Prompt), 400)}
	if q.Tool != "" {
		call := m.Code(q.Tool)
		if q.Args != "" {
			call += " " + m.Code(truncate(q.Args, 160))
		}
		lines = append(lines, m.Item("approve or deny "+call))
	}
	for _, o := range q.Options {
		lines = append(lines, m.Item(truncate(collapse(o), 160)))
	}
	if q.AskedOf != "" {
		lines = append(lines, m.Italic("asked of "+truncate(collapse(q.AskedOf), 80)))
	}
	return strings.Join(lines, "\n")
}

func layoutHeader(m Markup, v render.View) string {
	if v.Agent == "" {
		return ""
	}
	head := m.Bold(v.Agent)
	if q := v.Question; q != nil && q.Kind == "delegation" && v.Status == render.StatusParked {
		return head + " " + m.Italic("(waiting on "+strings.TrimPrefix(q.Tool, "tag_")+")")
	}
	if label := StatusLabel(v.Status); label != "" {
		head += " " + m.Italic("("+label+")")
	}
	return head
}

func layoutTools(m Markup, v render.View) string {
	var lines []string
	for _, t := range v.Tools {
		name := t.Name
		if name == "" {
			name = "tool"
		}
		line := m.Code(name)
		if t.Args != "" {
			line += " " + m.Code(truncate(t.Args, 160))
		}
		switch {
		case t.Error != "":
			line += " -> " + m.Bold("error") + ": " + truncate(collapse(t.Error), 160)
		case t.Done:
			line += " -> " + truncate(collapse(t.Result), 160)
		case v.Status == render.StatusParked:
			// A call in the batch a run parked on has not run and will not
			// until a person answers.
			line += " " + m.Italic("waiting")
		default:
			line += " " + m.Italic("running")
		}
		lines = append(lines, m.Item(line))
	}
	return strings.Join(lines, "\n")
}

// layoutCitations renders footnotes, which is the whole point of carrying
// citations as their own event kind: the agent's text refers to [1], and the
// reader needs [1] to be a link they can follow on the surface they are reading.
func layoutCitations(m Markup, v render.View) string {
	var lines []string
	for _, c := range v.Citations {
		label := c.Title
		if label == "" {
			label = c.URI
		}
		text := label
		if c.URI != "" {
			text = m.Link(label, c.URI)
		}
		line := "[" + c.Ref() + "] " + text
		if c.Snippet != "" {
			line += " " + m.Italic(truncate(collapse(c.Snippet), 200))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func layoutActions(m Markup, v render.View) string {
	var lines []string
	for _, a := range v.Actions {
		line := a.Summary
		if line == "" {
			line = "changed something"
		}
		if a.Address != "" {
			line += " (" + m.Link(a.Address, a.Address) + ")"
		}
		lines = append(lines, m.Item("changed: "+line))
	}
	return strings.Join(lines, "\n")
}

// Clamp bounds a rendered body to what a surface will accept. Every one of
// these APIs rejects an over-long body outright, which would cost the run its
// entire answer rather than its tail, so the tail goes instead.
//
// The head is kept, not the tail: a reader follows an answer from the top, and
// the run's full output is on the bus for anyone who wants all of it.
func Clamp(body string, max int) string {
	const marker = "\n\n[truncated]"
	if max <= 0 || len(body) <= max {
		return body
	}
	if max <= len(marker) {
		return cut(body, max)
	}
	return cut(body, max-len(marker)) + marker
}

// collapse folds whitespace so that a multi-line tool result or snippet cannot
// break the one-line-per-item layout it is being placed into.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncate bounds a fragment that came from outside. Every surface has a body
// limit, and one tool that returned a megabyte must not cost the run its answer.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "..."
	if max <= len(ellipsis) {
		return cut(s, max)
	}
	return cut(s, max-len(ellipsis)) + ellipsis
}

// cut shortens s to at most max bytes, on a rune boundary: a body ending
// mid-rune is rejected as invalid UTF-8 by more than one of these APIs.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8Start(s[end]) {
		end--
	}
	return s[:end]
}

// utf8Start reports whether b begins a UTF-8 encoded rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
