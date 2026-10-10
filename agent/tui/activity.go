package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/urmzd/saige/agent/types"
)

// ── Activity log ────────────────────────────────────────────────────

type activityKind int

const (
	activityUser     activityKind = iota // a message the user sent
	activityText                         // assistant text (accumulates)
	activityThinking                     // reasoning text (accumulates)
	activityTool                         // one tool call, updated in place from start to result
	activityMarker                       // approval required
	activityUsage                        // token usage
	activityNotice                       // handoff, route, citation, or interrupt note
	activityError                        // a run ended with an error
	activityStopped                      // a run was stopped or cut short
	activityMedia                        // media the model produced
	activityCitation                     // a citation, shown as a footnote
	activityRefusal                      // the model declined (accumulates)
)

// toolStatus is the lifecycle of one tool call.
type toolStatus int

const (
	toolPending toolStatus = iota // the model is still writing the arguments
	toolReady                     // arguments complete, not yet executing
	toolRunning                   // executing
	toolDone                      // finished successfully
	toolFailed                    // finished with an error
	toolStopped                   // the run ended before the call finished
)

type activityEntry struct {
	kind activityKind

	// Tool calls.
	callID   string
	name     string
	agent    bool // the call delegates to a sub-agent or streams nested output
	args     map[string]any
	argsErr  string
	argsDone bool
	status   toolStatus
	errMsg   string
	result   string

	content *strings.Builder  // text, thinking, refusal, and nested sub-agent output
	usage   *types.UsageDelta // usage entries
	markers []types.Marker    // marker entries
	text    string            // user, notice, citation, error, and stopped entries
	steer   bool              // user entry injected into an active run

	media     *mediaInfo  // media entries
	toolMedia []mediaInfo // media a tool call returned
	ordinal   int         // citation footnote number

	// born is when the entry appeared, for the fade-in.
	born time.Time

	// final is set once the turn holding a text entry has ended; only final
	// text is rendered as markdown, so a half-written block never reflows.
	final         bool
	rendered      string
	renderedWidth int
}

// activity is the transcript shared by Runner and StreamModel. It turns the
// delta stream into entries that are updated in place, so each tool call is
// one line whose status changes instead of a series of disconnected lines.
type activity struct {
	entries   []activityEntry
	calls     map[string]int // tool call ID to entry index
	parts     map[int]int    // part index of a streaming tool call or media part to entry index
	subAgents map[string]bool
	// footnotes numbers citations that arrive without an ordinal.
	footnotes int
	// now stamps new entries; nil means time.Now.
	now func() time.Time
}

// push appends e, stamped with the time it appeared.
func (a *activity) push(e activityEntry) {
	if a.now != nil {
		e.born = a.now()
	} else {
		e.born = time.Now()
	}
	a.entries = append(a.entries, e)
}

func newActivity(subAgents []string) activity {
	a := activity{calls: make(map[string]int), parts: make(map[int]int), subAgents: make(map[string]bool)}
	for _, s := range subAgents {
		a.subAgents[s] = true
	}
	return a
}

// delegationPrefixes are the tool name prefixes the agent uses for sub-agent
// delegation and handoff tools.
var delegationPrefixes = []string{"delegate_to_", "handoff_to_"}

// agentToolName reports whether a tool call delegates to a sub-agent, and the
// name to show for it.
func (a *activity) agentToolName(name string) (string, bool) {
	for _, p := range delegationPrefixes {
		if rest, ok := strings.CutPrefix(name, p); ok {
			return rest, true
		}
	}
	if a.subAgents[name] {
		return name, true
	}
	return name, false
}

// addUser records a message the user sent.
func (a *activity) addUser(text string, steer bool) {
	a.push(activityEntry{kind: activityUser, text: text, steer: steer})
}

// addNotice records a one-line note.
func (a *activity) addNotice(kind activityKind, text string) {
	a.push(activityEntry{kind: kind, text: text})
}

// appendText adds s to the open entry of kind, starting a new one when the
// last entry is something else.
func (a *activity) appendText(kind activityKind, s string) {
	if n := len(a.entries); n > 0 && a.entries[n-1].kind == kind && !a.entries[n-1].final {
		a.entries[n-1].content.WriteString(s)
		return
	}
	b := &strings.Builder{}
	b.WriteString(s)
	a.push(activityEntry{kind: kind, content: b})
}

// tool returns the entry for a tool call ID, creating one named name when the
// stream never announced the call (replays and forced calls skip the start).
func (a *activity) tool(id, name string) *activityEntry {
	if idx, ok := a.calls[id]; ok && id != "" {
		e := &a.entries[idx]
		if e.name == "" {
			e.name = name
		}
		return e
	}
	a.push(activityEntry{kind: activityTool, callID: id, name: name})
	idx := len(a.entries) - 1
	if id != "" {
		a.calls[id] = idx
	}
	e := &a.entries[idx]
	_, e.agent = a.agentToolName(name)
	return e
}

// isCancellation reports whether err means the run was stopped on request.
func isCancellation(err error) bool {
	return errors.Is(err, types.ErrStreamCanceled)
}

// apply folds one delta into the transcript.
func (a *activity) apply(d types.Delta) {
	switch d := d.(type) {
	case types.PartStart, types.PartDelta, types.PartEnd:
		a.applyPart(d)

	case types.ToolExecStartDelta:
		e := a.tool(d.ToolCallID, d.Name)
		e.argsDone = true
		e.status = toolRunning

	case types.ToolExecDelta:
		a.applyNested(d)

	case types.ToolExecEndDelta:
		e := a.tool(d.ToolCallID, d.Name)
		e.argsDone = true
		e.result = d.Result
		for _, p := range d.Parts {
			if types.IsMedia(p) {
				e.toolMedia = append(e.toolMedia, mediaOf(p))
			}
		}
		if d.Error != "" {
			e.status = toolFailed
			e.errMsg = d.Error
		} else {
			e.status = toolDone
		}

	case types.MarkerDelta:
		a.push(activityEntry{
			kind:    activityMarker,
			callID:  d.ToolCallID,
			name:    d.ToolName,
			args:    d.Arguments,
			markers: d.Markers,
		})

	case types.UsageDelta:
		usage := d
		a.push(activityEntry{kind: activityUsage, usage: &usage})

	case types.HandoffDelta:
		from := d.From
		if from == "" {
			from = "entry agent"
		}
		text := fmt.Sprintf("handoff: %s to %s", from, d.To)
		if d.Reason != "" {
			text += " (" + d.Reason + ")"
		}
		a.addNotice(activityNotice, text)

	case types.RouteDelta:
		a.addNotice(activityNotice, formatRoute(d))

	case types.CitationDelta:
		a.addCitation(d.Citation)

	case types.InterruptedDelta:
		a.addNotice(activityNotice, "interrupted: "+d.Reason)

	case types.TruncatedDelta:
		a.addNotice(activityStopped, "response cut short: "+d.Reason)

	case types.ErrorDelta:
		// A cancellation is not a failure; the consumer that asked for it
		// decides how to show the stop.
		if !isCancellation(d.Error) {
			a.addNotice(activityError, errorText(d.Error))
		}
	}
}

// applyPart folds a model part delta into the transcript.
func (a *activity) applyPart(d types.Delta) {
	switch d := d.(type) {
	case types.PartDelta:
		switch {
		case d.Text != "":
			a.appendText(activityText, d.Text)
		case d.Thinking != "":
			a.appendText(activityThinking, d.Thinking)
		case d.Refusal != "":
			a.appendText(activityRefusal, d.Refusal)
		case len(d.Data) > 0 || d.Transcript != "":
			if e := a.mediaAt(d.Index); e != nil {
				e.media.size += int64(len(d.Data))
				e.media.transcript += d.Transcript
			}
		}

	case types.PartStart:
		if isMediaKind(d.Kind) {
			a.push(activityEntry{kind: activityMedia, media: &mediaInfo{kind: d.Kind, mediaType: d.MediaType, streaming: true}})
			if a.parts == nil {
				a.parts = map[int]int{}
			}
			a.parts[d.Index] = len(a.entries) - 1
			return
		}
		if d.Kind != types.KindToolCall {
			return
		}
		e := a.tool(d.ID, d.Name)
		e.status = toolPending
		if a.parts == nil {
			a.parts = map[int]int{}
		}
		a.parts[d.Index] = a.calls[d.ID]
		if d.ID == "" {
			a.parts[d.Index] = len(a.entries) - 1
		}

	case types.PartEnd:
		a.endPart(d)
	}
}

// endPart folds the end of a model part into the transcript.
func (a *activity) endPart(d types.PartEnd) {
	switch p := d.Part.(type) {
	case types.CitationPart:
		a.addCitation(p.Citation)
		return
	case types.RefusalPart:
		if n := len(a.entries); p.Text != "" && (n == 0 || a.entries[n-1].kind != activityRefusal || a.entries[n-1].final) {
			a.appendText(activityRefusal, p.Text)
		}
		if n := len(a.entries); n > 0 && a.entries[n-1].kind == activityRefusal {
			a.entries[n-1].text = p.Category
			a.entries[n-1].final = true
		}
		return
	}
	if d.Part != nil && types.IsMedia(d.Part) {
		e := a.mediaAt(d.Index)
		if e == nil {
			a.push(activityEntry{kind: activityMedia, media: &mediaInfo{}})
			e = &a.entries[len(a.entries)-1]
		}
		delete(a.parts, d.Index)
		size := e.media.size
		*e.media = mediaOf(d.Part)
		if e.media.size == 0 {
			e.media.size = size
		}
		return
	}
	idx, ok := a.parts[d.Index]
	if !ok {
		return
	}
	delete(a.parts, d.Index)
	e := &a.entries[idx]
	if e.kind != activityTool {
		return
	}
	if call, ok := d.Part.(types.ToolCallPart); ok {
		e.args = call.Arguments
		e.argsErr = call.ArgumentsError
	}
	e.argsDone = true
	if e.status == toolPending {
		e.status = toolReady
	}
}

// mediaAt returns the media entry streaming at part index i, or nil.
func (a *activity) mediaAt(i int) *activityEntry {
	idx, ok := a.parts[i]
	if !ok || a.entries[idx].kind != activityMedia {
		return nil
	}
	return &a.entries[idx]
}

// addCitation records a citation as a footnote. One the run did not number
// gets the next number of the transcript's own.
func (a *activity) addCitation(c types.Citation) {
	n := c.Ordinal
	if n <= 0 {
		a.footnotes++
		n = a.footnotes
	}
	a.push(activityEntry{kind: activityCitation, ordinal: n, text: citationLabel(c)})
}

// isMediaKind reports whether parts of kind k carry media.
func isMediaKind(k types.PartKind) bool {
	switch k {
	case types.KindImage, types.KindAudio, types.KindVideo, types.KindDocument, types.KindFile,
		types.KindImageOut, types.KindAudioOut, types.KindVideoOut:
		return true
	}
	return false
}

// applyNested attributes a delta from inside a tool call (a sub-agent run or a
// streaming tool) to the outermost call, however deeply it is nested.
func (a *activity) applyNested(d types.ToolExecDelta) {
	path, inner := types.FlattenDelta(d)
	if len(path) == 0 {
		return
	}
	e := a.tool(path[0], "")
	e.agent = true
	if e.content == nil {
		e.content = &strings.Builder{}
	}
	line := func(s string) {
		if e.content.Len() > 0 && !strings.HasSuffix(e.content.String(), "\n") {
			e.content.WriteString("\n")
		}
		e.content.WriteString(s)
		e.content.WriteString("\n")
	}
	switch in := inner.(type) {
	case types.PartDelta:
		e.content.WriteString(in.Text)
	case types.PartStart:
		if in.Kind == types.KindToolCall {
			line(iconTool + " " + in.Name)
		}
	case types.ToolExecEndDelta:
		if in.Error != "" {
			line(iconError + " " + in.Name + ": " + in.Error)
		}
	case types.ErrorDelta:
		line(iconError + " " + errorText(in.Error))
	}
}

// finish ends the current turn: open text becomes final, and any tool call
// still in flight is marked stopped so it does not spin forever.
func (a *activity) finish() {
	for i := range a.entries {
		e := &a.entries[i]
		switch e.kind {
		case activityText, activityThinking, activityRefusal:
			e.final = true
		case activityMedia:
			e.media.streaming = false
		case activityTool:
			if e.status == toolPending || e.status == toolReady || e.status == toolRunning {
				e.status = toolStopped
			}
		}
	}
}

// renderMarkdown caches the markdown rendering of every final text entry at
// width. It runs in Update, never in View, so a frame never builds a renderer.
func (a *activity) renderMarkdown(width int) {
	for i := range a.entries {
		e := &a.entries[i]
		if e.kind != activityText || !e.final || e.content == nil {
			continue
		}
		if e.rendered != "" && e.renderedWidth == width {
			continue
		}
		e.rendered = renderMarkdownWidth(e.content.String(), width)
		e.renderedWidth = width
	}
}

// text returns the assistant text of every entry, in order.
func (a *activity) text() string {
	var b strings.Builder
	for _, e := range a.entries {
		if e.kind == activityText && e.content != nil {
			b.WriteString(e.content.String())
		}
	}
	return b.String()
}

// ── Rendering ───────────────────────────────────────────────────────

// logRenderer renders an activity log.
type logRenderer struct {
	entries []activityEntry
	// spin is the spinner frame to draw for work in progress.
	spin     string
	template Template
	// thinking adds a spinner line while a run is active and nothing has
	// arrived yet.
	thinking bool
	// expandThinking shows reasoning in full instead of collapsed, as
	// Template.ShowThinking does.
	expandThinking bool
	// animate fades new entries in, judged at now.
	animate bool
	now     time.Time
}

func (lr logRenderer) renderLog() string {
	var b strings.Builder
	for _, e := range lr.entries {
		if !lr.animate {
			lr.renderEntry(&b, e)
			continue
		}
		var eb strings.Builder
		lr.renderEntry(&eb, e)
		b.WriteString(fade(eb.String(), e.born, lr.now, lr.animate))
	}
	if lr.thinking && lr.template.ShowSpinner {
		fmt.Fprintf(&b, "  %s %s\n", lr.spin, thinkingStyle.Render("Thinking..."))
	}
	return b.String()
}

// shows reports whether renderEntry draws anything for e under the
// template, so counts of the transcript match what is on screen.
func (lr logRenderer) shows(e activityEntry) bool {
	t := lr.template
	switch e.kind {
	case activityText:
		return e.content != nil && e.content.Len() > 0 && (e.final || t.ShowStreamText)
	case activityThinking:
		return e.content != nil && e.content.Len() > 0
	case activityMedia:
		return e.media != nil
	case activityRefusal:
		return e.content != nil
	case activityTool:
		if e.agent {
			return t.ShowAgents
		}
		return t.ShowToolCalls
	case activityMarker:
		return t.ShowMarkers
	case activityUsage:
		return t.ShowUsage && e.usage != nil
	case activityNotice:
		return t.ShowRouting
	}
	return true
}

// visible counts the entries renderEntry draws.
func (lr logRenderer) visible(entries []activityEntry) int {
	n := 0
	for _, e := range entries {
		if lr.shows(e) {
			n++
		}
	}
	return n
}

func (lr logRenderer) renderEntry(b *strings.Builder, e activityEntry) {
	if !lr.shows(e) {
		return
	}
	switch e.kind {
	case activityUser:
		label := "you"
		if e.steer {
			label = "you (steer)"
		}
		fmt.Fprintf(b, "%s %s\n", promptStyle.Render(label+" >"), e.text)
	case activityText:
		lr.renderText(b, e)
	case activityThinking:
		lr.renderThinking(b, e)
	case activityMedia:
		if e.media != nil {
			fmt.Fprintf(b, "  %s\n", mediaStyle.Render(iconMedia+" "+e.media.label()))
		}
	case activityCitation:
		fmt.Fprintf(b, "  %s\n", footnoteStyle.Render(fmt.Sprintf("[%d] %s", e.ordinal, e.text)))
	case activityRefusal:
		if e.content != nil {
			label := iconRefusal + " declined"
			if e.text != "" {
				label += " (" + e.text + ")"
			}
			fmt.Fprintf(b, "  %s %s\n", refusalStyle.Render(label+":"), strings.TrimSpace(e.content.String()))
		}
	case activityTool:
		lr.renderTool(b, e)
	case activityMarker:
		if lr.template.ShowMarkers {
			fmt.Fprintf(b, "  %s\n", markerStyle.Render(fmt.Sprintf("%s Approval required: %s", iconMarker, e.name)))
		}
	case activityUsage:
		if lr.template.ShowUsage && e.usage != nil {
			fmt.Fprintf(b, "  %s\n", FormatUsage(e.usage.PromptTokens, e.usage.CompletionTokens, e.usage.Latency.String()))
		}
	case activityNotice:
		if lr.template.ShowRouting {
			fmt.Fprintf(b, "  %s\n", usageStyle.Render(e.text))
		}
	case activityError:
		fmt.Fprintf(b, "  %s\n", statusError.Render(iconError+" Error: "+e.text))
	case activityStopped:
		fmt.Fprintf(b, "  %s\n", stoppedStyle.Render(iconStopped+" "+e.text))
	}
}

// renderThinking draws reasoning collapsed to one line, or in full when the
// template or the user expands it.
func (lr logRenderer) renderThinking(b *strings.Builder, e activityEntry) {
	if e.content == nil || e.content.Len() == 0 {
		return
	}
	text := strings.TrimRight(e.content.String(), "\n")
	if lr.template.ShowThinking || lr.expandThinking {
		fmt.Fprintf(b, "  %s\n", thinkingStyle.Render(iconExpanded+" thinking"))
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(b, "    %s\n", thinkingStyle.Render(line))
		}
		return
	}
	words := len(strings.Fields(text))
	label := fmt.Sprintf("%s thinking (%d words, ctrl+t expands)", iconCollapsed, words)
	if !e.final {
		label = fmt.Sprintf("%s thinking %s (%d words)", iconCollapsed, lr.spin, words)
	}
	fmt.Fprintf(b, "  %s\n", thinkingStyle.Render(label))
}

func (lr logRenderer) renderText(b *strings.Builder, e activityEntry) {
	if e.content == nil || e.content.Len() == 0 {
		return
	}
	if e.final && lr.template.RenderMarkdown && e.rendered != "" {
		b.WriteString(strings.TrimRight(e.rendered, "\n"))
		b.WriteString("\n")
		return
	}
	if !e.final && !lr.template.ShowStreamText {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(e.content.String(), "\n"), "\n") {
		fmt.Fprintf(b, "  %s\n", line)
	}
}

func (lr logRenderer) renderTool(b *strings.Builder, e activityEntry) {
	name, isAgent := e.name, e.agent
	if isAgent {
		name = strings.TrimPrefix(strings.TrimPrefix(name, "delegate_to_"), "handoff_to_")
		if !lr.template.ShowAgents {
			return
		}
	} else if !lr.template.ShowToolCalls {
		return
	}

	style, icon := toolCallStyle, iconTool
	if isAgent {
		style, icon = agentDelegateStyle, iconAgent
	}
	line := "  " + style.Render(icon) + " " + style.Render(name)
	if lr.template.ShowToolArgs && e.args != nil {
		line += " " + usageStyle.Render(summarizeArgs(e.args, 100))
	}
	switch e.status {
	case toolPending, toolReady:
		line += " " + usageStyle.Render("...")
	case toolRunning:
		line += " " + lr.spin
	case toolDone:
		line += " " + statusDone.Render(iconDone)
	case toolFailed:
		line += " " + statusError.Render(iconError+" "+e.errMsg)
	case toolStopped:
		line += " " + stoppedStyle.Render("stopped")
	}
	b.WriteString(line)
	b.WriteString("\n")
	if e.argsErr != "" {
		fmt.Fprintf(b, "    %s\n", statusError.Render("invalid arguments: "+e.argsErr))
	}

	if e.content != nil && e.content.Len() > 0 {
		prefix := agentPrefixStyle.Render(fmt.Sprintf("    [%s] ", name))
		for _, l := range strings.Split(strings.TrimRight(e.content.String(), "\n"), "\n") {
			if strings.TrimSpace(l) != "" {
				fmt.Fprintf(b, "%s%s\n", prefix, agentOutputStyle.Render(l))
			}
		}
	}
	if lr.template.ShowToolResults && e.status == toolDone && e.result != "" {
		for _, l := range headLines(e.result, 5) {
			fmt.Fprintf(b, "    %s\n", agentOutputStyle.Render(l))
		}
	}
	for _, m := range e.toolMedia {
		fmt.Fprintf(b, "    %s\n", mediaStyle.Render(iconMedia+" "+m.label()))
	}
}

// ── Formatting helpers ──────────────────────────────────────────────

// errorText returns err's message, or a placeholder for a nil error.
func errorText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func formatRoute(d types.RouteDelta) string {
	parts := []string{"route:"}
	if d.Provider != "" || d.Model != "" {
		parts = append(parts, strings.Trim(d.Provider+"/"+d.Model, "/"))
	}
	if d.Profile != "" {
		parts = append(parts, "profile "+d.Profile)
	}
	if d.Experiment != "" {
		parts = append(parts, fmt.Sprintf("experiment %s/%s", d.Experiment, d.Variant))
	}
	if d.Reason != "" {
		parts = append(parts, "("+d.Reason+")")
	}
	return strings.Join(parts, " ")
}

// citationLabel is a citation's title and URI.
func citationLabel(c types.Citation) string {
	label := c.Title
	if label == "" {
		label = c.URI
	} else if c.URI != "" {
		label += " " + c.URI
	}
	if label == "" {
		label = "(untitled source)"
	}
	return label
}

// encodeArgs encodes tool arguments as JSON with sorted keys and without HTML
// escaping, optionally indented.
func encodeArgs(args map[string]any, indent bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(args); err != nil {
		return fmt.Sprintf("%v", args)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// summarizeArgs renders tool arguments on one line, cut to at most max runes.
func summarizeArgs(args map[string]any, max int) string {
	return truncateRunes(encodeArgs(args, false), max)
}

// FormatArgs renders tool arguments as indented JSON for an approval prompt,
// keeping at most maxLines lines so a large payload cannot push the prompt
// off screen. A nil or empty map renders as "{}".
func FormatArgs(args map[string]any, maxLines int) string {
	if len(args) == 0 {
		return "{}"
	}
	lines := strings.Split(encodeArgs(args, true), "\n")
	if maxLines > 0 && len(lines) > maxLines {
		hidden := len(lines) - maxLines
		lines = append(lines[:maxLines], fmt.Sprintf("... %d more lines", hidden))
	}
	return strings.Join(lines, "\n")
}

// truncateRunes cuts s to at most max runes, marking the cut with "...".
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

// headLines returns the first n lines of s, with a count of the rest.
func headLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return lines
	}
	return append(lines[:n:n], fmt.Sprintf("... %d more lines", len(lines)-n))
}

// ── Markdown ────────────────────────────────────────────────────────

// markdownRenderers caches one glamour renderer per wrap width. Building a
// renderer loads and parses a style sheet, which is too slow to repeat for
// every frame.
var markdownRenderers = struct {
	sync.Mutex
	byWidth map[int]*glamour.TermRenderer
}{byWidth: make(map[int]*glamour.TermRenderer)}

// markdownStyle picks the glamour style once per process. Detecting the
// terminal background queries the terminal, which must not happen while a
// bubbletea program is reading its input, so the answer is cached; lipgloss
// caches its own background check the same way.
var markdownStyle = sync.OnceValue(func() string {
	if fi, err := os.Stdout.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return styles.NoTTYStyle
	}
	if lipgloss.HasDarkBackground() {
		return styles.DarkStyle
	}
	return styles.LightStyle
})

// renderMarkdownWidth renders md for a terminal of the given width. It falls
// back to the raw text when rendering fails.
func renderMarkdownWidth(md string, width int) string {
	if width < 20 {
		width = 20
	}
	markdownRenderers.Lock()
	defer markdownRenderers.Unlock()
	r, ok := markdownRenderers.byWidth[width]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithStandardStyle(markdownStyle()), glamour.WithWordWrap(width))
		if err != nil {
			return md
		}
		markdownRenderers.byWidth[width] = r
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return out
}
