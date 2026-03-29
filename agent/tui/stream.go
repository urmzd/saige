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
	"github.com/charmbracelet/glamour"
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

// ── Activity log ────────────────────────────────────────────────────

type activityKind int

const (
	activityToolCall    activityKind = iota // ⚙ tool call started
	activityToolResult                      // tool call completed
	activityAgentStart                      // ▶ delegating to sub-agent
	activityAgentOutput                     // streaming sub-agent text (accumulates)
	activityAgentDone                       // ✓/✗ sub-agent complete
	activityMarker                          // ⚠ approval required
	activityText                            // coordinator streaming text (accumulates)
	activityUsage                           // ⏱ token usage
)

type activityEntry struct {
	kind      activityKind
	agentName string
	toolName  string
	content   *strings.Builder // for activityAgentOutput and activityText
	status    agentStatus      // for agent entries
	errMsg    string
	usage     *types.UsageDelta // for activityUsage
}

// ── Agent tracking ──────────────────────────────────────────────────

type agentStatus int

const (
	agentPending agentStatus = iota
	agentRunning
	agentDone
	agentError
)

// ── Shared renderLog ────────────────────────────────────────────────

// logRenderer holds state needed for rendering the activity log.
type logRenderer struct {
	log          []activityEntry
	spinner      spinner.Model
	synthesizing bool
	streaming    bool     // true while deltas are still arriving (show spinner/partial text)
	template     Template // controls which activity kinds are rendered
}

// renderLog builds the activity log content.
func (lr logRenderer) renderLog() string {
	var b strings.Builder

	for _, entry := range lr.log {
		lr.renderEntry(&b, entry)
	}

	if len(lr.log) == 0 && lr.streaming && lr.template.ShowSpinner {
		fmt.Fprintf(&b, "  %s %s\n", lr.spinner.View(), thinkingStyle.Render("Thinking..."))
	}

	return b.String()
}

func (lr logRenderer) renderEntry(b *strings.Builder, entry activityEntry) {
	switch entry.kind {
	case activityToolCall:
		lr.renderToolCall(b, entry)
	case activityToolResult:
		lr.renderToolResult(b, entry)
	case activityAgentStart:
		lr.renderAgentStart(b, entry)
	case activityAgentOutput:
		lr.renderAgentOutput(b, entry)
	case activityAgentDone:
		lr.renderAgentDone(b, entry)
	case activityMarker:
		lr.renderMarker(b, entry)
	case activityText:
		lr.renderText(b, entry)
	case activityUsage:
		lr.renderUsage(b, entry)
	}
}

func (lr logRenderer) renderToolCall(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowToolCalls {
		return
	}
	fmt.Fprintf(b, "  %s %s\n",
		toolCallStyle.Render(iconTool),
		toolCallStyle.Render(entry.toolName))
}

func (lr logRenderer) renderToolResult(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowToolCalls {
		return
	}
	if entry.errMsg != "" {
		fmt.Fprintf(b, "  %s %s %s\n",
			statusError.Render(iconError),
			toolCallStyle.Render(entry.toolName),
			statusError.Render(entry.errMsg))
	} else {
		fmt.Fprintf(b, "  %s %s\n",
			statusDone.Render(iconDone),
			toolCallStyle.Render(entry.toolName))
	}
}

func (lr logRenderer) renderAgentStart(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowAgents {
		return
	}
	fmt.Fprintf(b, "  %s %s\n",
		agentDelegateStyle.Render(iconAgent),
		agentDelegateStyle.Render(entry.agentName))
}

func (lr logRenderer) renderAgentOutput(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowAgents {
		return
	}
	if entry.content != nil && entry.content.Len() > 0 {
		text := entry.content.String()
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		prefix := agentPrefixStyle.Render(fmt.Sprintf("    [%s] ", entry.agentName))
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				fmt.Fprintf(b, "%s%s\n", prefix, agentOutputStyle.Render(line))
			}
		}
	}
	if entry.status == agentRunning {
		fmt.Fprintf(b, "    %s %s\n", lr.spinner.View(),
			statusRunning.Render(entry.agentName+"..."))
	}
}

func (lr logRenderer) renderAgentDone(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowAgents {
		return
	}
	if entry.status == agentError {
		fmt.Fprintf(b, "  %s %s %s\n",
			statusError.Render(iconError),
			agentDelegateStyle.Render(entry.agentName),
			statusError.Render(entry.errMsg))
	} else {
		fmt.Fprintf(b, "  %s %s\n",
			statusDone.Render(iconDone),
			agentDelegateStyle.Render(entry.agentName))
	}
}

func (lr logRenderer) renderMarker(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowMarkers {
		return
	}
	fmt.Fprintf(b, "  %s %s\n",
		markerStyle.Render(iconMarker),
		markerStyle.Render(fmt.Sprintf("Approval required: %s", entry.toolName)))
}

func (lr logRenderer) renderText(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowStreamText {
		return
	}
	if entry.content != nil && entry.content.Len() > 0 {
		if lr.synthesizing {
			fmt.Fprintf(b, "\n  %s %s\n", lr.spinner.View(),
				thinkingStyle.Render("Synthesizing..."))
		}
		text := entry.content.String()
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		start := 0
		if lr.streaming && len(lines) > 5 {
			start = len(lines) - 5
		}
		for _, line := range lines[start:] {
			fmt.Fprintf(b, "    %s\n", line)
		}
	}
}

func (lr logRenderer) renderUsage(b *strings.Builder, entry activityEntry) {
	if !lr.template.ShowUsage {
		return
	}
	if entry.usage != nil {
		fmt.Fprintf(b, "  %s %s\n",
			usageStyle.Render(iconUsage),
			usageStyle.Render(fmt.Sprintf("%d prompt + %d completion tokens, %s",
				entry.usage.PromptTokens, entry.usage.CompletionTokens, entry.usage.Latency)))
	}
}

// ── Bubbletea messages ──────────────────────────────────────────────

type deltaMsg struct {
	delta types.Delta
}

type streamDoneMsg struct{}

// ── StreamModel ─────────────────────────────────────────────────────

// StreamModel is a bubbletea model that consumes a delta channel from
// a saige EventStream and displays real-time progress for tool calls
// and sub-agent executions using a scrollable activity log.
type StreamModel struct {
	header      AgentHeader
	template    Template
	deltaCh     <-chan types.Delta
	finalReport *strings.Builder
	spinner     spinner.Model
	viewport    viewport.Model
	err         error
	ready       bool // viewport sized
	width       int

	// Activity log
	log          []activityEntry
	toolCallIdx  map[string]int // toolCallID → index in log for agent output entries
	hasAgents    bool           // true once any ToolExecStartDelta seen
	synthesizing bool           // true once coordinator text starts after agents
}

// NewStreamModel creates a StreamModel that reads deltas from ch and
// displays the given header info. An optional template controls which
// activity kinds are rendered; zero value uses TemplateDefault.
func NewStreamModel(header AgentHeader, ch <-chan types.Delta, tmpl ...Template) StreamModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	t := TemplateDefault
	if len(tmpl) > 0 && tmpl[0].Name != "" {
		t = tmpl[0]
	}
	return StreamModel{
		header:      header,
		template:    t,
		deltaCh:     ch,
		finalReport: &strings.Builder{},
		spinner:     s,
		viewport:    viewport.New(80, 20),
		toolCallIdx: make(map[string]int),
	}
}

// FinalReport returns the accumulated coordinator output text.
func (m StreamModel) FinalReport() string {
	return m.finalReport.String()
}

// Err returns any error encountered during the stream.
func (m StreamModel) Err() error {
	return m.err
}

// ── tea.Model implementation ────────────────────────────────────────

func (m StreamModel) Init() tea.Cmd {
	return tea.Batch(
		listenForDelta(m.deltaCh),
		m.spinner.Tick,
	)
}

func (m StreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.viewport.Width = msg.Width
		headerH := lipgloss.Height(renderHeader(m.header, msg.Width))
		m.viewport.Height = msg.Height - headerH - 1
		m.ready = true
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case streamDoneMsg:
		return m, tea.Quit

	case deltaMsg:
		return m.handleDelta(msg.delta)
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m StreamModel) handleDelta(d types.Delta) (tea.Model, tea.Cmd) {
	switch d := d.(type) {
	case types.ToolCallStartDelta:
		m.log = append(m.log, activityEntry{
			kind:     activityToolCall,
			toolName: d.Name,
		})

	case types.ToolExecStartDelta:
		m.hasAgents = true
		m.log = append(m.log, activityEntry{
			kind:      activityAgentStart,
			agentName: d.Name,
		})
		idx := len(m.log)
		m.log = append(m.log, activityEntry{
			kind:      activityAgentOutput,
			agentName: d.Name,
			content:   &strings.Builder{},
			status:    agentRunning,
		})
		m.toolCallIdx[d.ToolCallID] = idx

	case types.ToolExecDelta:
		if idx, ok := m.toolCallIdx[d.ToolCallID]; ok {
			entry := &m.log[idx]
			if inner, ok := d.Inner.(types.TextContentDelta); ok {
				entry.content.WriteString(inner.Content)
			}
		}

	case types.ToolExecEndDelta:
		if idx, ok := m.toolCallIdx[d.ToolCallID]; ok {
			entry := &m.log[idx]
			if d.Error != "" {
				entry.status = agentError
				entry.errMsg = d.Error
			} else {
				entry.status = agentDone
			}
			m.log = append(m.log, activityEntry{
				kind:      activityAgentDone,
				agentName: entry.agentName,
				status:    entry.status,
				errMsg:    entry.errMsg,
			})
		}

	case types.ToolCallEndDelta:
		// Find the matching tool call entry and mark it done
		for i := len(m.log) - 1; i >= 0; i-- {
			if m.log[i].kind == activityToolCall {
				m.log = append(m.log, activityEntry{
					kind:     activityToolResult,
					toolName: m.log[i].toolName,
				})
				break
			}
		}

	case types.MarkerDelta:
		m.log = append(m.log, activityEntry{
			kind:     activityMarker,
			toolName: d.ToolName,
		})

	case types.UsageDelta:
		usage := d // copy
		m.log = append(m.log, activityEntry{
			kind:  activityUsage,
			usage: &usage,
		})

	case types.TextContentDelta:
		m.finalReport.WriteString(d.Content)
		if m.hasAgents {
			m.synthesizing = true
		}
		if len(m.log) > 0 && m.log[len(m.log)-1].kind == activityText {
			m.log[len(m.log)-1].content.WriteString(d.Content)
		} else {
			entry := activityEntry{
				kind:    activityText,
				content: &strings.Builder{},
			}
			entry.content.WriteString(d.Content)
			m.log = append(m.log, entry)
		}

	case types.ErrorDelta:
		m.err = d.Error
		return m, tea.Quit

	case types.DoneDelta:
		return m, tea.Quit
	}

	lr := logRenderer{log: m.log, spinner: m.spinner, synthesizing: m.synthesizing, streaming: true, template: m.template}
	m.viewport.SetContent(lr.renderLog())
	m.viewport.GotoBottom()

	return m, listenForDelta(m.deltaCh)
}

func (m StreamModel) View() string {
	var b strings.Builder

	if m.template.ShowHeader {
		b.WriteString(renderHeader(m.header, m.width))
		b.WriteString("\n")
	}

	lr := logRenderer{log: m.log, spinner: m.spinner, synthesizing: m.synthesizing, streaming: true, template: m.template}
	if m.ready {
		m.viewport.SetContent(lr.renderLog())
		b.WriteString(m.viewport.View())
	} else {
		b.WriteString(lr.renderLog())
	}

	return b.String()
}

// ── Delta bridge ────────────────────────────────────────────────────

func listenForDelta(ch <-chan types.Delta) tea.Cmd {
	return func() tea.Msg {
		delta, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return deltaMsg{delta: delta}
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

// StreamVerboseWithTemplate consumes deltas with template-controlled output.
// verboseStreamer holds state for the verbose streaming output.
type verboseStreamer struct {
	w                    io.Writer
	tmpl                 Template
	agentNames           map[string]string // toolCallID → name
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
	if vs.tmpl.ShowToolCalls {
		fmt.Fprintln(vs.w, FormatToolCall(d.Name))
	}
}

func (vs *verboseStreamer) handleToolExecStart(d types.ToolExecStartDelta) {
	vs.ensureNewline()
	vs.agentNames[d.ToolCallID] = d.Name
	vs.agentNewLine[d.ToolCallID] = true
	vs.agentStarted[d.ToolCallID] = false
	if vs.tmpl.ShowAgents {
		fmt.Fprintln(vs.w, FormatDelegateStart(d.Name))
	}
}

func (vs *verboseStreamer) handleToolExecDelta(d types.ToolExecDelta) {
	if !vs.tmpl.ShowAgents {
		return
	}
	inner, ok := d.Inner.(types.TextContentDelta)
	if !ok {
		return
	}
	name := vs.agentNames[d.ToolCallID]
	vs.agentStarted[d.ToolCallID] = true
	content := inner.Content

	if vs.agentNewLine[d.ToolCallID] {
		_, _ = fmt.Fprint(vs.w, FormatAgentOutput(name, ""))
		vs.agentNewLine[d.ToolCallID] = false
	}
	if strings.Contains(content, "\n") {
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
			vs.agentNewLine[d.ToolCallID] = true
		}
	} else {
		fmt.Fprint(vs.w, content)
	}
}

func (vs *verboseStreamer) handleToolExecEnd(d types.ToolExecEndDelta) {
	if !vs.tmpl.ShowAgents {
		return
	}
	name := vs.agentNames[d.ToolCallID]
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

func StreamVerboseWithTemplate(header AgentHeader, ch <-chan types.Delta, w io.Writer, tmpl Template) VerboseResult {
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
		case types.ToolCallStartDelta:
			vs.handleToolCallStart(d)
		case types.ToolCallArgumentDelta:
			// argument JSON fragments — skip in verbose mode
		case types.ToolCallEndDelta:
			// tool call fully parsed — logged at exec start
		case types.ToolExecStartDelta:
			vs.handleToolExecStart(d)
		case types.ToolExecDelta:
			vs.handleToolExecDelta(d)
		case types.ToolExecEndDelta:
			vs.handleToolExecEnd(d)
		case types.MarkerDelta:
			vs.handleMarker(d)
		case types.UsageDelta:
			if tmpl.ShowUsage {
				vs.ensureNewline()
				fmt.Fprintln(w, FormatUsage(d.PromptTokens, d.CompletionTokens, d.Latency.String()))
			}
		case types.ErrorDelta:
			vs.ensureNewline()
			fmt.Fprintln(w, statusError.Render(fmt.Sprintf("%s Error: %v", iconError, d.Error)))
			return VerboseResult{Text: vs.text.String(), Err: d.Error}
		case types.DoneDelta:
			vs.ensureNewline()
			return VerboseResult{Text: vs.text.String()}
		}
	}

	return VerboseResult{Text: vs.text.String()}
}

// ── Markdown rendering ──────────────────────────────────────────────

// RenderMarkdown renders markdown text as styled terminal output using glamour.
func RenderMarkdown(md string) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)
	if err != nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return out
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
