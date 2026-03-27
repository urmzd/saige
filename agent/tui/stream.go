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
}

// renderLog builds the activity log content.
func (lr logRenderer) renderLog() string {
	var b strings.Builder

	for _, entry := range lr.log {
		switch entry.kind {
		case activityToolCall:
			fmt.Fprintf(&b, "  %s %s\n",
				toolCallStyle.Render(iconTool),
				toolCallStyle.Render(entry.toolName))

		case activityToolResult:
			if entry.errMsg != "" {
				fmt.Fprintf(&b, "  %s %s %s\n",
					statusError.Render(iconError),
					toolCallStyle.Render(entry.toolName),
					statusError.Render(entry.errMsg))
			} else {
				fmt.Fprintf(&b, "  %s %s\n",
					statusDone.Render(iconDone),
					toolCallStyle.Render(entry.toolName))
			}

		case activityAgentStart:
			fmt.Fprintf(&b, "  %s %s\n",
				agentDelegateStyle.Render(iconAgent),
				agentDelegateStyle.Render(entry.agentName))

		case activityAgentOutput:
			if entry.content != nil && entry.content.Len() > 0 {
				text := entry.content.String()
				lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
				prefix := agentPrefixStyle.Render(fmt.Sprintf("    [%s] ", entry.agentName))
				for _, line := range lines {
					if strings.TrimSpace(line) != "" {
						fmt.Fprintf(&b, "%s%s\n", prefix, agentOutputStyle.Render(line))
					}
				}
			}
			if entry.status == agentRunning {
				fmt.Fprintf(&b, "    %s %s\n", lr.spinner.View(),
					statusRunning.Render(entry.agentName+"..."))
			}

		case activityAgentDone:
			if entry.status == agentError {
				fmt.Fprintf(&b, "  %s %s %s\n",
					statusError.Render(iconError),
					agentDelegateStyle.Render(entry.agentName),
					statusError.Render(entry.errMsg))
			} else {
				fmt.Fprintf(&b, "  %s %s\n",
					statusDone.Render(iconDone),
					agentDelegateStyle.Render(entry.agentName))
			}

		case activityMarker:
			fmt.Fprintf(&b, "  %s %s\n",
				markerStyle.Render(iconMarker),
				markerStyle.Render(fmt.Sprintf("Approval required: %s", entry.toolName)))

		case activityText:
			if entry.content != nil && entry.content.Len() > 0 {
				if lr.synthesizing {
					fmt.Fprintf(&b, "\n  %s %s\n", lr.spinner.View(),
						thinkingStyle.Render("Synthesizing..."))
				}
				text := entry.content.String()
				lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
				start := 0
				if len(lines) > 5 {
					start = len(lines) - 5
				}
				for _, line := range lines[start:] {
					fmt.Fprintf(&b, "    %s\n", line)
				}
			}

		case activityUsage:
			if entry.usage != nil {
				fmt.Fprintf(&b, "  %s %s\n",
					usageStyle.Render(iconUsage),
					usageStyle.Render(fmt.Sprintf("%d prompt + %d completion tokens, %s",
						entry.usage.PromptTokens, entry.usage.CompletionTokens, entry.usage.Latency)))
			}
		}
	}

	if len(lr.log) == 0 {
		fmt.Fprintf(&b, "  %s %s\n", lr.spinner.View(), thinkingStyle.Render("Thinking..."))
	}

	return b.String()
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
// displays the given header info.
func NewStreamModel(header AgentHeader, ch <-chan types.Delta) StreamModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	return StreamModel{
		header:      header,
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

	lr := logRenderer{log: m.log, spinner: m.spinner, synthesizing: m.synthesizing}
	m.viewport.SetContent(lr.renderLog())
	m.viewport.GotoBottom()

	return m, listenForDelta(m.deltaCh)
}

func (m StreamModel) View() string {
	var b strings.Builder

	b.WriteString(renderHeader(m.header, m.width))
	b.WriteString("\n")

	lr := logRenderer{log: m.log, spinner: m.spinner, synthesizing: m.synthesizing}
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
// to w. It does not require an interactive terminal. All delta types are
// logged for a complete trace.
func StreamVerbose(header AgentHeader, ch <-chan types.Delta, w io.Writer) VerboseResult {
	if w == nil {
		w = os.Stdout
	}

	// Print header
	fmt.Fprintln(w, renderHeader(header, 80))
	fmt.Fprintln(w)

	agentNames := map[string]string{}    // toolCallID → name
	agentNewLine := map[string]bool{}    // toolCallID → needs prefix on next chunk
	agentStarted := map[string]bool{}    // toolCallID → has received any text
	toolCallNames := map[string]string{} // toolCallID → tool name
	var text strings.Builder
	coordinatorStreaming := false

	ensureNewline := func() {
		if coordinatorStreaming {
			fmt.Fprintln(w)
			coordinatorStreaming = false
		}
	}

	for delta := range ch {
		switch d := delta.(type) {

		// ── LLM text streaming ──────────────────────────────────────
		case types.TextStartDelta:
			ensureNewline()

		case types.TextContentDelta:
			text.WriteString(d.Content)
			_, _ = fmt.Fprint(w, d.Content)
			coordinatorStreaming = true

		case types.TextEndDelta:
			ensureNewline()

		// ── LLM tool call streaming ─────────────────────────────────
		case types.ToolCallStartDelta:
			ensureNewline()
			toolCallNames[d.ID] = d.Name
			fmt.Fprintln(w, FormatToolCall(d.Name))

		case types.ToolCallArgumentDelta:
			// argument JSON fragments — skip in verbose mode

		case types.ToolCallEndDelta:
			// tool call fully parsed — logged at exec start

		// ── Tool execution ──────────────────────────────────────────
		case types.ToolExecStartDelta:
			ensureNewline()
			agentNames[d.ToolCallID] = d.Name
			agentNewLine[d.ToolCallID] = true
			agentStarted[d.ToolCallID] = false
			fmt.Fprintln(w, FormatDelegateStart(d.Name))

		case types.ToolExecDelta:
			if inner, ok := d.Inner.(types.TextContentDelta); ok {
				name := agentNames[d.ToolCallID]
				agentStarted[d.ToolCallID] = true
				content := inner.Content

				if agentNewLine[d.ToolCallID] {
					_, _ = fmt.Fprint(w, FormatAgentOutput(name, ""))
					agentNewLine[d.ToolCallID] = false
				}
				if strings.Contains(content, "\n") {
					prefix := FormatAgentOutput(name, "")
					lines := strings.Split(content, "\n")
					for i, line := range lines {
						if i > 0 {
							fmt.Fprint(w, prefix)
						}
						fmt.Fprint(w, line)
						if i < len(lines)-1 {
							fmt.Fprintln(w)
						}
					}
					if strings.HasSuffix(content, "\n") {
						agentNewLine[d.ToolCallID] = true
					}
				} else {
					fmt.Fprint(w, content)
				}
			}

		case types.ToolExecEndDelta:
			name := agentNames[d.ToolCallID]
			if agentStarted[d.ToolCallID] && !agentNewLine[d.ToolCallID] {
				fmt.Fprintln(w)
			}
			if d.Error != "" {
				fmt.Fprintln(w, FormatAgentError(name, d.Error))
			} else {
				fmt.Fprintln(w, FormatAgentDone(name))
			}

		// ── Markers ─────────────────────────────────────────────────
		case types.MarkerDelta:
			ensureNewline()
			fmt.Fprintln(w, FormatMarker(d.ToolName))
			for _, m := range d.Markers {
				fmt.Fprintln(w, markerDetailStyle.Render(
					fmt.Sprintf("  %s: %s", m.Kind, m.Message)))
			}

		// ── Metadata ────────────────────────────────────────────────
		case types.UsageDelta:
			ensureNewline()
			fmt.Fprintln(w, FormatUsage(d.PromptTokens, d.CompletionTokens, d.Latency.String()))

		// ── Terminal ────────────────────────────────────────────────
		case types.ErrorDelta:
			ensureNewline()
			fmt.Fprintln(w, statusError.Render(fmt.Sprintf("%s Error: %v", iconError, d.Error)))
			return VerboseResult{Text: text.String(), Err: d.Error}

		case types.DoneDelta:
			ensureNewline()
			return VerboseResult{Text: text.String()}
		}
	}

	return VerboseResult{Text: text.String()}
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
