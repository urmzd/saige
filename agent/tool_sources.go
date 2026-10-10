package agent

import (
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// toolSourcesHeader leads the text the model sees for a tool result that
// carries citations, so it can cite them by marker.
const toolSourcesHeader = "Sources (cite with the marker):"

// citeToolSources returns messages as the model is sent them: every tool
// result that carries numbered citations is led by a text part listing each
// source with its marker ("[1] Handbook <file://handbook.md>"), so the model
// can write the markers the run's citation registry resolves. The record is
// not changed: the tree keeps the result as the tool returned it, and only
// the request carries the list. Messages without such a result are returned
// as is.
func citeToolSources(messages []types.Message) []types.Message {
	var out []types.Message
	for i, msg := range messages {
		cited, changed := citeMessage(msg)
		if !changed {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		if out == nil {
			out = make([]types.Message, 0, len(messages))
			out = append(out, messages[:i]...)
		}
		out = append(out, cited)
	}
	if out == nil {
		return messages
	}
	return out
}

// citeMessage returns msg with its cited tool results led by their source
// list, and whether any result changed.
func citeMessage(msg types.Message) (types.Message, bool) {
	switch m := msg.(type) {
	case types.SystemMessage:
		parts, changed := citeParts(m.Parts)
		return types.SystemMessage{Parts: parts}, changed
	case types.UserMessage:
		parts, changed := citeParts(m.Parts)
		return types.UserMessage{Parts: parts}, changed
	}
	return msg, false
}

func citeParts[P types.Part](parts []P) ([]P, bool) {
	var out []P
	for i, p := range parts {
		r, ok := any(p).(types.ToolResultPart)
		if !ok {
			continue
		}
		legend := sourceLegend(r.Citations)
		if legend == "" {
			continue
		}
		if out == nil {
			out = slices.Clone(parts)
		}
		r.Parts = append([]types.ToolOutputPart{types.Text(legend)}, r.Parts...)
		out[i] = any(r).(P)
	}
	if out == nil {
		return parts, false
	}
	return out, true
}

// sourceLegend lists the numbered citations, one per line, each source once
// in the order the tool reported them. It is empty when none is numbered.
func sourceLegend(cites []types.Citation) string {
	var b strings.Builder
	seen := map[int]bool{}
	for _, c := range cites {
		if c.Ordinal <= 0 || seen[c.Ordinal] {
			continue
		}
		seen[c.Ordinal] = true
		if b.Len() == 0 {
			b.WriteString(toolSourcesHeader)
		}
		b.WriteString("\n")
		b.WriteString(c.Marker())
		if c.Title != "" {
			b.WriteString(" ")
			b.WriteString(c.Title)
		}
		if c.URI != "" {
			b.WriteString(" <")
			b.WriteString(c.URI)
			b.WriteString(">")
		}
	}
	return b.String()
}
