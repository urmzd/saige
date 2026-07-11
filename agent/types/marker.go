package types

import "context"

// Marker is a routing annotation attached to a tool. When a marked tool is
// invoked, the agent loop pauses, emits a MarkerDelta to the consumer, and
// waits for resolution before proceeding.
type Marker struct {
	Kind    string         // e.g. "human_approval", "audit", "rate_limit"
	Message string         // human-readable description of what's being gated
	Meta    map[string]any // arbitrary metadata for the consumer
}

// MarkedTool wraps a Tool with one or more Markers. It always implements
// RichTool so approval-wrapped rich results pass through intact.
type MarkedTool struct {
	Inner   Tool
	Markers []Marker
}

var _ RichTool = (*MarkedTool)(nil)

func (m *MarkedTool) Definition() ToolDef { return m.Inner.Definition() }

func (m *MarkedTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	return m.Inner.Execute(ctx, args)
}

// ExecuteRich implements RichTool. When the wrapped tool is itself a RichTool,
// its rich result passes through unchanged; a plain Tool's string result is
// wrapped in a text-only ToolResult, which the agent loop treats exactly like
// the plain Execute path (nil Blocks).
func (m *MarkedTool) ExecuteRich(ctx context.Context, args map[string]any) (ToolResult, error) {
	if rt, ok := m.Inner.(RichTool); ok {
		return rt.ExecuteRich(ctx, args)
	}
	text, err := m.Inner.Execute(ctx, args)
	return ToolResult{Text: text}, err
}

// WithMarkers wraps a tool with markers.
func WithMarkers(tool Tool, markers ...Marker) *MarkedTool {
	return &MarkedTool{Inner: tool, Markers: markers}
}
