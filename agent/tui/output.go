package tui

import (
	"io"
	"os"

	"github.com/urmzd/saige/agent/types"
)

// OutputHeader provides consistent identity info across all CLI commands.
type OutputHeader struct {
	Operation string // "chat", "ask", "rag search", "kg ingest", etc.
	Provider  string // LLM provider name (empty for non-agent commands)
	Context   string // extra info (model name, database, etc.)
}

// Output is the unified rendering interface for all saige CLI commands.
// Agent commands use Header + StreamDeltas; RAG/KG commands use Header + Result.
type Output interface {
	// Header renders command identity info.
	Header(h OutputHeader)

	// Result renders a structured Go value (JSON-serializable).
	Result(v any) error

	// StreamDeltas consumes an agent delta channel and renders progress.
	StreamDeltas(header AgentHeader, ch <-chan types.Delta) VerboseResult

	// Error renders an error message.
	Error(err error)

	// Status renders an informational status line.
	Status(msg string)
}

// ResolveOutput returns the appropriate Output implementation.
// When jsonMode is true, returns JSONOutput (for pipes/scripts).
// Otherwise returns StyledOutput (for human terminals).
func ResolveOutput(jsonMode bool, tmpl Template) Output {
	if jsonMode {
		return NewJSONOutput(os.Stdout, os.Stderr)
	}
	return NewStyledOutput(os.Stdout, os.Stderr, tmpl)
}

// ResolveOutputWriters is like ResolveOutput but allows custom writers.
func ResolveOutputWriters(jsonMode bool, tmpl Template, w, errW io.Writer) Output {
	if jsonMode {
		return NewJSONOutput(w, errW)
	}
	return NewStyledOutput(w, errW, tmpl)
}
