package workspace

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// Default spill thresholds, in bytes.
const (
	DefaultSpillMaxBytes     = 16 << 10
	DefaultSpillPreviewBytes = 2 << 10
)

// SpillOptions configures Spill.
type SpillOptions struct {
	// MaxBytes is the largest text the model receives whole. Larger text is
	// stored and replaced by a preview. 0 uses DefaultSpillMaxBytes.
	MaxBytes int
	// PreviewBytes is how much of a spilled text the model sees at most. The
	// preview ends at the last word boundary within it. 0 uses
	// DefaultSpillPreviewBytes; it is capped at MaxBytes.
	PreviewBytes int
}

func (o SpillOptions) withDefaults() SpillOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultSpillMaxBytes
	}
	if o.PreviewBytes <= 0 {
		o.PreviewBytes = DefaultSpillPreviewBytes
	}
	if o.PreviewBytes > o.MaxBytes {
		o.PreviewBytes = o.MaxBytes
	}
	return o
}

// Spill wraps tool so that a result larger than MaxBytes is stored in ws and
// replaced by a preview that names its saige-artifact:// URI. The model can
// page through the full result with scratch_read, so nothing is lost; the
// context only holds what it asked for.
//
// Approval markers stay outermost: spilling a types.MarkedTool returns a
// MarkedTool around the spilling tool, so the agent loop still sees them.
// Wrap ordinary tools only. Handoff, clarification, and sub-agent tools are
// recognized by their concrete type and must not be wrapped.
//
// When the store rejects the write, for example through a read-only view, the
// result is returned whole rather than cut. Inside a sub-agent the attached
// workspace is the child's scratch over the parent's, so the child's spills
// land in its own scratch and the parent reads them through the result.
func Spill(tool types.Tool, ws Workspace, opts SpillOptions) types.Tool {
	if mt, ok := tool.(*types.MarkedTool); ok {
		return types.WithMarkers(Spill(mt.Inner, ws, opts), mt.Markers...)
	}
	return &spillTool{inner: tool, ws: ws, opts: opts.withDefaults()}
}

// SpillAll wraps every tool with Spill.
func SpillAll(ws Workspace, opts SpillOptions, tools ...types.Tool) []types.Tool {
	out := make([]types.Tool, len(tools))
	for i, t := range tools {
		out[i] = Spill(t, ws, opts)
	}
	return out
}

type spillTool struct {
	inner types.Tool
	ws    Workspace
	opts  SpillOptions
}

var _ types.RichTool = (*spillTool)(nil)

func (s *spillTool) Definition() types.ToolDef { return s.inner.Definition() }

// Unwrap returns the wrapped tool.
func (s *spillTool) Unwrap() types.Tool { return s.inner }

func (s *spillTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	text, err := s.inner.Execute(ctx, args)
	if err != nil {
		return text, err
	}
	return s.spill(ctx, text), nil
}

func (s *spillTool) ExecuteRich(ctx context.Context, args map[string]any) (types.ToolResult, error) {
	rt, ok := s.inner.(types.RichTool)
	if !ok {
		text, err := s.Execute(ctx, args)
		return types.ToolResult{Text: text}, err
	}
	res, err := rt.ExecuteRich(ctx, args)
	if err != nil || res.IsError {
		return res, err
	}
	res.Text = s.spill(ctx, res.Text)
	if len(res.Blocks) > 0 {
		blocks := make([]types.ToolResultBlock, len(res.Blocks))
		for i, b := range res.Blocks {
			switch {
			case b.Kind == types.ToolResultBlockText && len(b.Text) > s.opts.MaxBytes:
				b.Text = s.spill(ctx, b.Text)
			case b.Kind == types.ToolResultBlockJSON && len(b.JSON) > s.opts.MaxBytes:
				b = types.ToolResultBlock{Kind: types.ToolResultBlockText, Text: s.spill(ctx, string(b.JSON))}
			}
			blocks[i] = b
		}
		res.Blocks = blocks
	}
	return res, nil
}

func (s *spillTool) spill(ctx context.Context, text string) string {
	if len(text) <= s.opts.MaxBytes {
		return text
	}
	// The workspace attached to the call wins, so a tool built for a
	// parent spills into a sub-agent's private scratch when a child runs
	// it. A nil ws spills only where a workspace is attached.
	w := s.ws
	if attached, ok := FromContext(ctx); ok {
		w = attached
	}
	if w == nil {
		return text
	}
	name := "tool-results/" + s.inner.Definition().Name + "/" + Digest([]byte(text))[:16]
	ref, err := w.Put(ctx, name, []byte(text), map[string]string{"tool": s.inner.Definition().Name})
	if err != nil {
		return text
	}
	// Cut at a word boundary so a value crossing the cut, such as an email
	// address, is not left as a fragment a redactor cannot recognize.
	preview := text[:cutAtBoundary(text, s.opts.PreviewBytes)]
	return fmt.Sprintf("%s\n\n[result truncated: showing %d of %d bytes. Full result: %s. Read more with %s using offset %d.]",
		preview, len(preview), len(text), ref.URI(), ReadToolName, len(preview))
}
