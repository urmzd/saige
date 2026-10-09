// Package tui provides a bubbletea-based progress UI for streaming agent
// deltas. It tracks tool calls, sub-agent executions, and markers with
// distinct icons and formatting, and provides verbose-mode helpers for
// non-TTY / debug output.
package tui

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/urmzd/saige/agent/types"
)

// ── Agent info header ──────────────────────────────────────────────

// AgentHeader holds display info for the TUI header panel.
type AgentHeader struct {
	Name      string
	Provider  string
	Tools     []string
	SubAgents []string
	CWD       string // working directory (shortened)
	User      string // current username
}

// renderHeader builds a bordered agent info panel.
func renderHeader(h AgentHeader, width int) string {
	if h.Name == "" && h.Provider == "" && len(h.Tools) == 0 && len(h.SubAgents) == 0 {
		return ""
	}

	var lines []string

	name := h.Name
	if name == "" {
		name = "Agent"
	}
	lines = append(lines, headerTitle.Render(name))

	if h.Provider != "" {
		lines = append(lines, headerLabel.Render("Provider: ")+headerValue.Render(h.Provider))
	}

	if len(h.Tools) > 0 {
		toolList := strings.Join(h.Tools, headerDim.Render(", "))
		lines = append(lines, headerLabel.Render("Tools:    ")+headerValue.Render(toolList))
	}

	if len(h.SubAgents) > 0 {
		agentList := strings.Join(h.SubAgents, headerDim.Render(", "))
		lines = append(lines, headerLabel.Render("Agents:   ")+headerValue.Render(agentList))
	}

	if h.CWD != "" {
		lines = append(lines, headerLabel.Render("CWD:      ")+headerValue.Render(h.CWD))
	}

	if h.User != "" {
		lines = append(lines, headerLabel.Render("User:     ")+headerValue.Render(h.User))
	}

	content := strings.Join(lines, "\n")

	style := headerBorder
	if width > 0 {
		style = style.Width(width - 2) // account for border
	}

	return style.Render(content)
}

// PopulateEnv fills the CWD and User fields of an AgentHeader from the environment.
func PopulateEnv(h *AgentHeader) {
	if dir, err := os.Getwd(); err == nil {
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home) {
			dir = "~" + dir[len(home):]
		}
		h.CWD = dir
	}
	if u, err := user.Current(); err == nil {
		h.User = u.Username
	}
}

// ── Bubbletea messages ──────────────────────────────────────────────

// deltaMsg carries one delta from stream generation gen. A model ignores
// deltas from a generation it is no longer reading, so a late delta from a
// finished run can never leak into the next one.
type deltaMsg struct {
	gen   int
	delta types.Delta
}

// streamDoneMsg reports that the delta channel of generation gen closed.
type streamDoneMsg struct{ gen int }

// ── StreamModel ─────────────────────────────────────────────────────

// StreamModel is a bubbletea model that consumes a delta channel from
// a saige EventStream and displays real-time progress for tool calls
// and sub-agent executions using a scrollable activity log.
type StreamModel struct {
	header   AgentHeader
	template Template
	deltaCh  <-chan types.Delta
	cancel   func()
	spinner  spinner.Model
	viewport viewport.Model
	err      error
	ready    bool // viewport sized
	width    int
	height   int
	done     bool

	act activity
}

// NewStreamModel creates a StreamModel that reads deltas from ch and
// displays the given header info. An optional template controls which
// activity kinds are rendered; zero value uses TemplateDefault.
func NewStreamModel(header AgentHeader, ch <-chan types.Delta, tmpl ...Template) StreamModel {
	t := TemplateDefault
	if len(tmpl) > 0 && tmpl[0].Name != "" {
		t = tmpl[0]
	}
	return StreamModel{
		header:   header,
		template: t,
		deltaCh:  ch,
		spinner:  newSpinner(),
		viewport: viewport.New(80, 20),
		act:      newActivity(header.SubAgents),
	}
}

// WithCancel returns a copy of m that calls cancel when the user quits before
// the stream ends, typically the stream's Cancel method. Without it, quitting
// only stops rendering and the run keeps going in the background.
func (m StreamModel) WithCancel(cancel func()) StreamModel {
	m.cancel = cancel
	return m
}

// FinalReport returns the accumulated coordinator output text.
func (m StreamModel) FinalReport() string {
	return m.act.text()
}

// Err returns any error encountered during the stream.
func (m StreamModel) Err() error {
	return m.err
}

func newSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	return s
}

// ── tea.Model implementation ────────────────────────────────────────

func (m StreamModel) Init() tea.Cmd {
	return tea.Batch(
		listenForDelta(0, m.deltaCh),
		m.spinner.Tick,
	)
}

func (m StreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == keyCtrlC || msg.String() == "q" {
			if !m.done && m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.refresh()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.refresh()
		return m, cmd

	case streamDoneMsg:
		m.done = true
		m.act.finish()
		return m, tea.Quit

	case deltaMsg:
		return m.handleDelta(msg.delta)
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m StreamModel) handleDelta(d types.Delta) (tea.Model, tea.Cmd) {
	m.act.apply(d)
	switch d := d.(type) {
	case types.ErrorDelta:
		m.err = d.Error
		m.done = true
		m.act.finish()
		m.refresh()
		return m, tea.Quit
	case types.DoneDelta:
		m.done = true
		m.act.finish()
		m.refresh()
		return m, tea.Quit
	}
	m.refresh()
	return m, listenForDelta(0, m.deltaCh)
}

// refresh sizes the viewport and sets its content, following the bottom
// unless the user has scrolled up.
func (m *StreamModel) refresh() {
	if m.ready {
		m.viewport.Width = m.width
		m.viewport.Height = viewportHeight(m.height, m.headerView(), "")
	}
	follow := m.viewport.AtBottom()
	m.viewport.SetContent(m.logView())
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m StreamModel) logView() string {
	lr := logRenderer{entries: m.act.entries, spinner: m.spinner, template: m.template, thinking: !m.done && len(m.act.entries) == 0}
	return lr.renderLog()
}

func (m StreamModel) headerView() string {
	if !m.template.ShowHeader {
		return ""
	}
	return renderHeader(m.header, m.width)
}

func (m StreamModel) View() string {
	var b strings.Builder
	if h := m.headerView(); h != "" {
		b.WriteString(h)
		b.WriteString("\n")
	}
	if m.ready {
		b.WriteString(m.viewport.View())
	} else {
		b.WriteString(m.logView())
	}
	return b.String()
}

// viewportHeight is the height left for the scrolling log once header and
// footer are drawn. It never drops below one line, so a short terminal shows
// a cramped log instead of a negative-height viewport.
func viewportHeight(total int, header, footer string) int {
	h := total
	if header != "" {
		h -= lipgloss.Height(header)
	}
	if footer != "" {
		h -= lipgloss.Height(footer)
	}
	if h < 1 {
		h = 1
	}
	return h
}

// ── Delta bridge ────────────────────────────────────────────────────

func listenForDelta(gen int, ch <-chan types.Delta) tea.Cmd {
	return func() tea.Msg {
		delta, ok := <-ch
		if !ok {
			return streamDoneMsg{gen: gen}
		}
		return deltaMsg{gen: gen, delta: delta}
	}
}

// ── Verbose-mode formatting helpers ─────────────────────────────────

func FormatDelegateStart(name string) string {
	return agentDelegateStyle.Render(fmt.Sprintf("%s Delegating to %s...", iconAgent, name))
}

func FormatAgentOutput(name, content string) string {
	return agentPrefixStyle.Render(fmt.Sprintf("[%s] ", name)) + content
}

func FormatAgentDone(name string) string {
	return statusDone.Render(fmt.Sprintf("%s %s complete", iconDone, name))
}

func FormatAgentError(name, errMsg string) string {
	return statusError.Render(fmt.Sprintf("%s %s error: %s", iconError, name, errMsg))
}

func FormatToolCall(name string) string {
	return toolCallStyle.Render(fmt.Sprintf("%s %s", iconTool, name))
}

func FormatToolResult(name string) string {
	return statusDone.Render(fmt.Sprintf("%s %s", iconDone, name))
}

func FormatToolError(name, errMsg string) string {
	return statusError.Render(fmt.Sprintf("%s %s: %s", iconError, name, errMsg))
}

func FormatMarker(toolName string) string {
	return markerStyle.Render(fmt.Sprintf("%s Approval required: %s", iconMarker, toolName))
}

func FormatUsage(prompt, completion int, latency string) string {
	return usageStyle.Render(fmt.Sprintf("%s %d prompt + %d completion tokens, %s", iconUsage, prompt, completion, latency))
}

// ── Non-interactive streaming ───────────────────────────────────────

// VerboseResult holds the outcome of a StreamVerbose run.
type VerboseResult struct {
	Text string // accumulated coordinator text output
	Err  error  // first error encountered, if any
}

// StreamVerbose consumes deltas from ch and writes styled progress output
// to w. It does not require an interactive terminal.
func StreamVerbose(header AgentHeader, ch <-chan types.Delta, w io.Writer) VerboseResult {
	return StreamVerboseWithTemplate(header, ch, w, TemplateDefault)
}

// verboseStreamer holds state for the verbose streaming output.
type verboseStreamer struct {
	w                    io.Writer
	tmpl                 Template
	act                  activity          // classifies tool calls as sub-agent delegations
	toolNames            map[string]string // toolCallID → tool name
	agentNames           map[string]string // toolCallID → sub-agent name, for delegations only
	agentNewLine         map[string]bool   // toolCallID → needs prefix on next chunk
	agentStarted         map[string]bool   // toolCallID → has received any text
	text                 strings.Builder
	coordinatorStreaming bool
}

func (vs *verboseStreamer) ensureNewline() {
	if vs.coordinatorStreaming {
		fmt.Fprintln(vs.w)
		vs.coordinatorStreaming = false
	}
}

func (vs *verboseStreamer) handleTextContent(d types.TextContentDelta) {
	vs.text.WriteString(d.Content)
	_, _ = fmt.Fprint(vs.w, d.Content)
	vs.coordinatorStreaming = true
}

func (vs *verboseStreamer) handleToolCallStart(d types.ToolCallStartDelta) {
	vs.ensureNewline()
	vs.toolNames[d.ID] = d.Name
	if _, isAgent := vs.act.agentToolName(d.Name); isAgent {
		return // announced as a delegation when it starts executing
	}
	if vs.tmpl.ShowToolCalls {
		fmt.Fprintln(vs.w, FormatToolCall(d.Name))
	}
}

func (vs *verboseStreamer) handleToolCallEnd(d types.ToolCallEndDelta) {
	if !vs.tmpl.ShowToolArgs || d.Arguments == nil {
		return
	}
	vs.ensureNewline()
	fmt.Fprintln(vs.w, usageStyle.Render("  "+summarizeArgs(d.Arguments, 200)))
}

func (vs *verboseStreamer) handleToolExecStart(d types.ToolExecStartDelta) {
	vs.ensureNewline()
	if d.Name != "" {
		vs.toolNames[d.ToolCallID] = d.Name
	}
	name, isAgent := vs.act.agentToolName(vs.toolNames[d.ToolCallID])
	if !isAgent {
		return
	}
	vs.startAgent(d.ToolCallID, name)
}

// startAgent begins nested output for a delegation or a streaming tool.
func (vs *verboseStreamer) startAgent(id, name string) {
	vs.agentNames[id] = name
	vs.agentNewLine[id] = true
	vs.agentStarted[id] = false
	if vs.tmpl.ShowAgents {
		fmt.Fprintln(vs.w, FormatDelegateStart(name))
	}
}

// handleToolExecDelta prints output from inside a tool call. Nested
// wrappers are flattened, so a sub-agent's own tool calls, its errors, and
// deeper delegations are all attributed to the outermost call.
func (vs *verboseStreamer) handleToolExecDelta(d types.ToolExecDelta) {
	path, inner := types.FlattenDelta(d)
	if len(path) == 0 {
		return
	}
	id := path[0]
	if _, ok := vs.agentNames[id]; !ok {
		// A tool that streams nested output is shown like a delegation.
		vs.ensureNewline()
		name, _ := vs.act.agentToolName(vs.toolNames[id])
		vs.startAgent(id, name)
	}
	if !vs.tmpl.ShowAgents {
		return
	}
	switch in := inner.(type) {
	case types.TextContentDelta:
		vs.agentText(id, in.Content)
	case types.ToolCallStartDelta:
		vs.agentLine(id, iconTool+" "+in.Name)
	case types.ToolExecEndDelta:
		if in.Error != "" {
			vs.agentLine(id, iconError+" "+in.Name+": "+in.Error)
		}
	case types.ErrorDelta:
		vs.agentLine(id, iconError+" "+errorText(in.Error))
	}
}

// agentText streams nested text, prefixing each new line with the name of
// the outermost call.
func (vs *verboseStreamer) agentText(id, content string) {
	name := vs.agentNames[id]
	vs.agentStarted[id] = true
	if vs.agentNewLine[id] {
		_, _ = fmt.Fprint(vs.w, FormatAgentOutput(name, ""))
		vs.agentNewLine[id] = false
	}
	if !strings.Contains(content, "\n") {
		fmt.Fprint(vs.w, content)
		return
	}
	prefix := FormatAgentOutput(name, "")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if i > 0 {
			fmt.Fprint(vs.w, prefix)
		}
		fmt.Fprint(vs.w, line)
		if i < len(lines)-1 {
			fmt.Fprintln(vs.w)
		}
	}
	if strings.HasSuffix(content, "\n") {
		vs.agentNewLine[id] = true
	}
}

// agentLine prints one complete line of nested activity, such as a
// sub-agent's tool call, on its own prefixed line.
func (vs *verboseStreamer) agentLine(id, text string) {
	if vs.agentStarted[id] && !vs.agentNewLine[id] {
		fmt.Fprintln(vs.w)
	}
	fmt.Fprintln(vs.w, FormatAgentOutput(vs.agentNames[id], agentOutputStyle.Render(text)))
	vs.agentStarted[id] = true
	vs.agentNewLine[id] = true
}

func (vs *verboseStreamer) handleToolExecEnd(d types.ToolExecEndDelta) {
	vs.ensureNewline()
	name, isAgent := vs.agentNames[d.ToolCallID]
	if !isAgent {
		if !vs.tmpl.ShowToolCalls {
			return
		}
		tool := d.Name
		if tool == "" {
			tool = vs.toolNames[d.ToolCallID]
		}
		if d.Error != "" {
			fmt.Fprintln(vs.w, FormatToolError(tool, d.Error))
			return
		}
		fmt.Fprintln(vs.w, FormatToolResult(tool))
		if vs.tmpl.ShowToolResults && d.Result != "" {
			for _, l := range headLines(d.Result, 5) {
				fmt.Fprintln(vs.w, agentOutputStyle.Render("    "+l))
			}
		}
		return
	}
	if !vs.tmpl.ShowAgents {
		return
	}
	if vs.agentStarted[d.ToolCallID] && !vs.agentNewLine[d.ToolCallID] {
		fmt.Fprintln(vs.w)
	}
	if d.Error != "" {
		fmt.Fprintln(vs.w, FormatAgentError(name, d.Error))
	} else {
		fmt.Fprintln(vs.w, FormatAgentDone(name))
	}
}

func (vs *verboseStreamer) handleMarker(d types.MarkerDelta) {
	if !vs.tmpl.ShowMarkers {
		return
	}
	vs.ensureNewline()
	fmt.Fprintln(vs.w, FormatMarker(d.ToolName))
	for _, m := range d.Markers {
		fmt.Fprintln(vs.w, markerDetailStyle.Render(
			fmt.Sprintf("  %s: %s", m.Kind, m.Message)))
	}
}

// StreamVerboseWithTemplate consumes deltas with template-controlled output.
func StreamVerboseWithTemplate(header AgentHeader, ch <-chan types.Delta, w io.Writer, tmpl Template) VerboseResult {
	return StreamVerboseResolving(header, ch, w, tmpl, nil)
}

// StreamVerboseResolving is StreamVerboseWithTemplate with inline marker
// resolution: each MarkerDelta is rendered and then passed to resolve before
// the next delta is read. A nil resolve only renders markers, which leaves
// the agent loop waiting until something else resolves them.
func StreamVerboseResolving(header AgentHeader, ch <-chan types.Delta, w io.Writer, tmpl Template, resolve MarkerResolver) VerboseResult {
	return streamVerbose(header, ch, w, tmpl, resolve, true)
}

// streamVerbose renders ch on w. printErr writes a terminal ErrorDelta on w
// as well as returning it; an Output passes false, because its caller
// reports the returned error through Output.Error on the error writer.
func streamVerbose(header AgentHeader, ch <-chan types.Delta, w io.Writer, tmpl Template, resolve MarkerResolver, printErr bool) VerboseResult {
	if w == nil {
		w = os.Stdout
	}

	if tmpl.ShowHeader {
		fmt.Fprintln(w, renderHeader(header, 80))
		fmt.Fprintln(w)
	}

	vs := &verboseStreamer{
		w:            w,
		tmpl:         tmpl,
		act:          newActivity(header.SubAgents),
		toolNames:    make(map[string]string),
		agentNames:   make(map[string]string),
		agentNewLine: make(map[string]bool),
		agentStarted: make(map[string]bool),
	}

	for delta := range ch {
		switch d := delta.(type) {
		case types.TextStartDelta:
			vs.ensureNewline()
		case types.TextContentDelta:
			vs.handleTextContent(d)
		case types.TextEndDelta:
			vs.ensureNewline()
		case types.ThinkingContentDelta:
			if tmpl.ShowThinking {
				_, _ = fmt.Fprint(w, thinkingStyle.Render(d.Content))
				vs.coordinatorStreaming = true
			}
		case types.ThinkingEndDelta:
			vs.ensureNewline()
		case types.ToolCallStartDelta:
			vs.handleToolCallStart(d)
		case types.ToolCallArgumentDelta:
			// argument JSON fragments: the complete arguments arrive on ToolCallEndDelta
		case types.ToolCallEndDelta:
			vs.handleToolCallEnd(d)
		case types.ToolExecStartDelta:
			vs.handleToolExecStart(d)
		case types.ToolExecDelta:
			vs.handleToolExecDelta(d)
		case types.ToolExecEndDelta:
			vs.handleToolExecEnd(d)
		case types.MarkerDelta:
			vs.handleMarker(d)
			if resolve != nil {
				resolve(d)
			}
		case types.UsageDelta:
			if tmpl.ShowUsage {
				vs.ensureNewline()
				fmt.Fprintln(w, FormatUsage(d.PromptTokens, d.CompletionTokens, d.Latency.String()))
			}
		case types.HandoffDelta, types.RouteDelta, types.CitationDelta, types.InterruptedDelta:
			if tmpl.ShowRouting {
				var note activity
				note.apply(d)
				vs.ensureNewline()
				fmt.Fprintln(w, usageStyle.Render(note.entries[0].text))
			}
		case types.TruncatedDelta:
			vs.ensureNewline()
			fmt.Fprintln(w, stoppedStyle.Render(iconStopped+" response cut short: "+d.Reason))
		case types.ErrorDelta:
			vs.ensureNewline()
			if printErr {
				fmt.Fprintln(w, statusError.Render(fmt.Sprintf("%s Error: %v", iconError, d.Error)))
			}
			return VerboseResult{Text: vs.text.String(), Err: d.Error}
		case types.DoneDelta:
			vs.ensureNewline()
			return VerboseResult{Text: vs.text.String()}
		}
	}

	return VerboseResult{Text: vs.text.String()}
}

// ── Markdown rendering ──────────────────────────────────────────────

// RenderMarkdown renders markdown text as styled terminal output using glamour,
// wrapped at 80 columns.
func RenderMarkdown(md string) string {
	return renderMarkdownWidth(md, 80)
}

// RenderReport renders a titled section with the report body formatted as markdown.
func RenderReport(title, body string) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(reportTitleStyle.Render(title))
	b.WriteString("\n")
	b.WriteString(reportDividerStyle.Render(strings.Repeat(iconSeparator, 60)))
	b.WriteString("\n")
	b.WriteString(RenderMarkdown(body))
	return b.String()
}
