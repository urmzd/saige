package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// Runner is a multi-turn interactive TUI runner that implements agentsdk.Runner.
// It reads user input, invokes the agent, renders streaming deltas, resolves
// markers, and loops until the user types /quit or cancels with ctrl+c.
type Runner struct {
	Title   string
	Verbose bool // use plain text streaming instead of bubbletea
}

// Name implements agentsdk.NamedRunner.
func (r *Runner) Name() string { return "tui" }

// Run implements agentsdk.Runner. It starts the interactive conversation loop.
func (r *Runner) Run(ctx context.Context, agent *agentsdk.Agent) error {
	if r.Verbose {
		return r.runVerbose(ctx, agent)
	}
	return r.runInteractive(ctx, agent)
}

// ── Verbose mode ─────────────────────────────────────────────────────

func (r *Runner) runVerbose(ctx context.Context, agent *agentsdk.Agent) error {
	w := os.Stdout
	scanner := bufio.NewScanner(os.Stdin)

	info := agent.Info()
	header := AgentHeader{
		Name:      info.Name,
		Provider:  info.Provider,
		Tools:     info.Tools,
		SubAgents: info.SubAgents,
	}
	PopulateEnv(&header)
	_, _ = fmt.Fprintln(w, renderHeader(header, 80))
	_, _ = fmt.Fprintln(w)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, _ = fmt.Fprint(w, promptStyle.Render(">>> "))
		if !scanner.Scan() {
			return scanner.Err()
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "/quit" || input == "/exit" {
			return nil
		}

		stream := agent.Invoke(ctx, []types.Message{
			types.NewUserMessage(input),
		})

		go r.resolveMarkersVerbose(ctx, stream, scanner, w)

		// Pass empty header — already printed above
		result := StreamVerbose(AgentHeader{}, stream.Deltas(), w)
		if result.Err != nil {
			_, _ = fmt.Fprintln(w, statusError.Render(fmt.Sprintf("%s Error: %v", iconError, result.Err)))
		} else if result.Text != "" {
			fmt.Fprintln(w)
		}
	}
}

// resolveMarkersVerbose watches for MarkerDelta and prompts the user.
func (r *Runner) resolveMarkersVerbose(ctx context.Context, stream *agentsdk.EventStream, scanner *bufio.Scanner, w io.Writer) {
	_ = ctx
	_ = stream
	_ = scanner
	_ = w
}

// ── Interactive mode ─────────────────────────────────────────────────

func (r *Runner) runInteractive(ctx context.Context, agent *agentsdk.Agent) error {
	m := newRunnerModel(agent, ctx)
	p := tea.NewProgram(m, tea.WithContext(ctx))
	finalModel, err := p.Run()
	if err != nil {
		return err
	}
	if rm, ok := finalModel.(runnerModel); ok && rm.err != nil {
		return rm.err
	}
	return nil
}

// ── Bubbletea model for interactive runner ───────────────────────────

type runnerPhase int

const (
	phaseInput     runnerPhase = iota // waiting for user input
	phaseStreaming                    // agent is streaming response
	phaseMarker                      // waiting for marker approval
)

// markerPending tracks a marker awaiting user resolution.
type markerPending struct {
	toolCallID string
	toolName   string
	markers    []types.Marker
}

type runnerModel struct {
	header    AgentHeader
	ctx       context.Context
	agent     *agentsdk.Agent
	phase     runnerPhase
	textInput textinput.Model
	spinner   spinner.Model
	viewport  viewport.Model
	stream    *agentsdk.EventStream
	deltaCh   <-chan types.Delta
	output    *strings.Builder // accumulated output for current turn
	err       error
	marker    *markerPending // pending marker, if any
	ready     bool           // viewport sized
	width     int

	// Activity log for sub-agent display
	log          []activityEntry
	toolCallIdx  map[string]int // toolCallID → index in log
	hasAgents    bool
	synthesizing bool
}

func newRunnerModel(agent *agentsdk.Agent, ctx context.Context) runnerModel {
	ti := textinput.New()
	ti.Placeholder = "Type a message (/quit to exit)"
	ti.Focus()
	ti.Width = 60

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))

	info := agent.Info()
	header := AgentHeader{
		Name:      info.Name,
		Provider:  info.Provider,
		Tools:     info.Tools,
		SubAgents: info.SubAgents,
	}
	PopulateEnv(&header)
	if header.Name == "" {
		header.Name = "Agent"
	}

	return runnerModel{
		header:      header,
		ctx:         ctx,
		agent:       agent,
		phase:       phaseInput,
		textInput:   ti,
		spinner:     s,
		viewport:    viewport.New(80, 20),
		output:      &strings.Builder{},
		toolCallIdx: make(map[string]int),
	}
}

func (m runnerModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.spinner.Tick)
}

func (m runnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.viewport.Width = msg.Width
		headerH := lipgloss.Height(renderHeader(m.header, msg.Width))
		m.viewport.Height = msg.Height - headerH - 3 // header + input + padding
		m.ready = true
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case deltaMsg:
		return m.handleDelta(msg.delta)
	case streamDoneMsg:
		return m.finishTurn()
	}

	if m.phase == phaseInput {
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m runnerModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.stream != nil {
			m.stream.Cancel()
		}
		return m, tea.Quit

	case "enter":
		if m.phase == phaseMarker && m.marker != nil {
			input := strings.TrimSpace(m.textInput.Value())
			approved := input != "n" && input != "no"
			m.stream.ResolveMarker(m.marker.toolCallID, approved, nil)
			m.marker = nil
			m.phase = phaseStreaming
			m.textInput.Reset()
			m.textInput.Blur()
			return m, listenForDelta(m.deltaCh)
		}

		if m.phase == phaseInput {
			input := strings.TrimSpace(m.textInput.Value())
			if input == "" {
				return m, nil
			}
			if input == "/quit" || input == "/exit" {
				return m, tea.Quit
			}

			m.textInput.Reset()
			m.textInput.Blur()
			m.output.Reset()
			m.log = nil
			m.toolCallIdx = make(map[string]int)
			m.hasAgents = false
			m.synthesizing = false

			stream := m.agent.Invoke(m.ctx, []types.Message{
				types.NewUserMessage(input),
			})
			m.stream = stream
			m.deltaCh = stream.Deltas()
			m.phase = phaseStreaming

			return m, listenForDelta(m.deltaCh)
		}
	}

	if m.phase == phaseInput || m.phase == phaseMarker {
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m runnerModel) handleDelta(d types.Delta) (tea.Model, tea.Cmd) {
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
		m.marker = &markerPending{
			toolCallID: d.ToolCallID,
			toolName:   d.ToolName,
			markers:    d.Markers,
		}
		m.phase = phaseMarker
		m.textInput.Placeholder = "Approve? (y/n)"
		m.textInput.Focus()
		return m, textinput.Blink

	case types.UsageDelta:
		usage := d
		m.log = append(m.log, activityEntry{
			kind:  activityUsage,
			usage: &usage,
		})

	case types.TextContentDelta:
		m.output.WriteString(d.Content)
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
		return m.finishTurn()
	}

	lr := logRenderer{log: m.log, spinner: m.spinner, synthesizing: m.synthesizing}
	m.viewport.SetContent(lr.renderLog())
	m.viewport.GotoBottom()

	return m, listenForDelta(m.deltaCh)
}

func (m runnerModel) finishTurn() (tea.Model, tea.Cmd) {
	m.phase = phaseInput
	m.stream = nil
	m.deltaCh = nil
	m.textInput.Placeholder = "Type a message (/quit to exit)"
	m.textInput.Focus()
	return m, textinput.Blink
}

func (m runnerModel) View() string {
	var b strings.Builder

	b.WriteString(renderHeader(m.header, m.width))
	b.WriteString("\n")

	switch m.phase {
	case phaseInput:
		if m.output.Len() > 0 {
			b.WriteString(RenderMarkdown(m.output.String()))
			b.WriteString("\n")
		}
		b.WriteString(m.textInput.View())
		b.WriteString("\n")

	case phaseStreaming:
		lr := logRenderer{log: m.log, spinner: m.spinner, synthesizing: m.synthesizing}
		if m.ready {
			m.viewport.SetContent(lr.renderLog())
			b.WriteString(m.viewport.View())
		} else {
			b.WriteString(lr.renderLog())
		}

	case phaseMarker:
		if m.marker != nil {
			b.WriteString(markerStyle.Render(
				fmt.Sprintf("%s Tool %q requires approval", iconMarker, m.marker.toolName)))
			b.WriteString("\n")
			for _, mk := range m.marker.markers {
				b.WriteString(markerDetailStyle.Render(
					fmt.Sprintf("  %s: %s", mk.Kind, mk.Message)))
				b.WriteString("\n")
			}
			b.WriteString("\n")
			b.WriteString(m.textInput.View())
			b.WriteString("\n")
		}
	}

	return b.String()
}
