package tui

import "github.com/charmbracelet/lipgloss"

// ── Header styles ───────────────────────────────────────────────────

var (
	headerBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("12")).
			Padding(0, 1).
			MarginBottom(1)

	headerTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))

	headerLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Bold(true)

	headerValue = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15"))

	headerDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))
)

// ── Activity log icons ──────────────────────────────────────────────

const (
	iconTool      = "⚙" // tool call
	iconAgent     = "▶" // agent delegation
	iconDone      = "✓" // success
	iconError     = "✗" // failure
	iconMarker    = "⚠" // approval required
	iconUsage     = "⏱" // token usage
	iconSeparator = "─" // section divider
	iconStopped   = "■" // run stopped or cut short
	iconQueued    = "…" // message waiting for the run
	iconMedia     = "◆" // media placeholder
	iconRefusal   = "⊘" // the model declined
	iconCollapsed = "▸" // collapsed reasoning
	iconExpanded  = "▾" // expanded reasoning
)

// ── Activity log styles ─────────────────────────────────────────────

var (
	// Tool call events
	toolCallStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("13")). // magenta
			Bold(true)

	// Agent delegation events
	agentDelegateStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("14")). // cyan
				Bold(true)

	agentPrefixStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("14")). // cyan
				Bold(true)

	agentOutputStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8")) // dim

	// Status styles
	statusDone = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")) // green

	statusError = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")) // red

	// Marker / approval styles
	markerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")). // yellow
			Bold(true)

	markerDetailStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("11"))

	// Text / thinking
	thinkingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Italic(true)

	// Usage / metadata
	usageStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	// Report styles
	reportTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("15")). // white
				Background(lipgloss.Color("4")).  // blue bg
				Padding(0, 1)

	reportDividerStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))

	// Stopped runs and the queued-message strip
	stoppedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")) // yellow

	queuedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")) // dim

	// Media placeholders, citation footnotes, and refusals
	mediaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("14")) // cyan

	footnoteStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")) // dim

	refusalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")). // yellow
			Bold(true)

	// New entries fade in from faint
	fadeStyle = lipgloss.NewStyle().Faint(true)

	// Filter and follow indicators
	filterBadgeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("11")) // yellow

	indicatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12")). // blue
			Bold(true)

	// Prompt (runner input)
	promptStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12")).
			Bold(true)
)
