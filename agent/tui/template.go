package tui

// Template controls the visual output format of the TUI.
// Different templates let you validate different agents with distinct
// levels of detail.
type Template struct {
	Name string

	// What to show
	ShowHeader     bool // render the agent info header
	ShowToolCalls  bool // show tool call/result entries
	ShowAgents     bool // show sub-agent delegation entries
	ShowUsage      bool // show token usage
	ShowMarkers    bool // show approval markers
	ShowSpinner    bool // show spinner while thinking
	ShowStreamText bool // show partial text during streaming

	// How to render final output
	RenderMarkdown bool // render final output as glamour markdown
}

// Built-in templates.
var (
	// TemplateDefault shows all chrome: header, tool calls, agents, usage,
	// and renders final output as terminal markdown.
	TemplateDefault = Template{
		Name:           "default",
		ShowHeader:     true,
		ShowToolCalls:  true,
		ShowAgents:     true,
		ShowUsage:      true,
		ShowMarkers:    true,
		ShowSpinner:    true,
		ShowStreamText: true,
		RenderMarkdown: true,
	}

	// TemplateMinimal strips all chrome — just the response text and
	// approval prompts. Good for quick validation or piping.
	TemplateMinimal = Template{
		Name:           "minimal",
		ShowHeader:     false,
		ShowToolCalls:  false,
		ShowAgents:     false,
		ShowUsage:      false,
		ShowMarkers:    true, // always show approval requests
		ShowSpinner:    true,
		ShowStreamText: true,
		RenderMarkdown: false,
	}

	// TemplateDetailed shows everything including all activity log entries.
	// Useful for debugging agent behavior.
	TemplateDetailed = Template{
		Name:           "detailed",
		ShowHeader:     true,
		ShowToolCalls:  true,
		ShowAgents:     true,
		ShowUsage:      true,
		ShowMarkers:    true,
		ShowSpinner:    true,
		ShowStreamText: true,
		RenderMarkdown: true,
	}
)

// TemplateByName returns a built-in template by name.
// Returns TemplateDefault for unrecognized names.
func TemplateByName(name string) Template {
	switch name {
	case "minimal":
		return TemplateMinimal
	case "detailed":
		return TemplateDetailed
	case "default", "":
		return TemplateDefault
	default:
		return TemplateDefault
	}
}
