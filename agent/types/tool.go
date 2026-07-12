package types

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ToolDef describes a tool's schema for the LLM.
type ToolDef struct {
	Name        string
	Description string
	Parameters  ParameterSchema
}

// ParameterSchema is a JSON-Schema-like definition for tool parameters.
type ParameterSchema struct {
	Type       string
	Required   []string
	Properties map[string]PropertyDef
}

// PropertyDef describes a single parameter property using JSON Schema fields.
type PropertyDef struct {
	Type        string                 `json:"type"`
	Description string                 `json:"description,omitempty"`
	Enum        []string               `json:"enum,omitempty"`
	Items       *PropertyDef           `json:"items,omitempty"`
	Properties  map[string]PropertyDef `json:"properties,omitempty"`
	Required    []string               `json:"required,omitempty"`
	Default     any                    `json:"default,omitempty"`
}

// Tool is the base interface all tools implement.
type Tool interface {
	Definition() ToolDef
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// ── Rich (multi-modal) tool output ──────────────────────────────────

// ToolResultBlockKind enumerates the kinds of a ToolResultBlock.
type ToolResultBlockKind string

const (
	ToolResultBlockText  ToolResultBlockKind = "text"
	ToolResultBlockImage ToolResultBlockKind = "image"
	ToolResultBlockFile  ToolResultBlockKind = "file" // non-image binary (e.g. PDF, CSV)
	ToolResultBlockJSON  ToolResultBlockKind = "json" // structured data; serialized to text for the LLM
)

// ToolResultBlock is one content block inside a ToolResult. Exactly one payload
// field is meaningful per Kind. Data is tagged json:"-" (like FileContent.Data)
// so tree serialization persists only metadata + URI, never large raw bytes.
type ToolResultBlock struct {
	Kind      ToolResultBlockKind `json:"kind"`
	Text      string              `json:"text,omitempty"`       // Kind == text
	MediaType MediaType           `json:"media_type,omitempty"` // Kind == image|file
	URI       string              `json:"uri,omitempty"`        // Kind == image|file: re-resolvable source location
	Filename  string              `json:"filename,omitempty"`   // Kind == image|file: display name
	Data      []byte              `json:"-"`                    // Kind == image|file: raw bytes (NOT persisted)
	JSON      json.RawMessage     `json:"json,omitempty"`       // Kind == json: structured payload
}

// ToolResult is the structured, multi-modal output of a tool execution and the
// return value of the optional RichTool interface. Text is the MANDATORY
// plain-text projection: it is what text-only providers send to the LLM and what
// humans see in ToolExecEndDelta.Result. Blocks carries the full multi-modal
// payload for providers that support it; nil Blocks behaves exactly like a plain
// string result.
type ToolResult struct {
	Text    string            // required human/LLM text projection (never empty for a successful result)
	Blocks  []ToolResultBlock // optional rich content
	IsError bool              // true when this result represents a tool error
}

// RichTool is an OPTIONAL extension interface. A tool that implements it can
// return structured multi-modal output. The agent loop prefers ExecuteRich when
// a tool implements RichTool; otherwise it calls Execute and wraps the string.
// RichTool embeds Tool, so a RichTool is always a valid Tool: Execute remains
// the text-only fallback (typically `r, err := ExecuteRich(...); return r.Text, err`).
type RichTool interface {
	Tool
	ExecuteRich(ctx context.Context, args map[string]any) (ToolResult, error)
}

// TextResult builds a plain-text ToolResult (no rich blocks).
func TextResult(text string) ToolResult { return ToolResult{Text: text} }

// ImageResult builds a ToolResult carrying a text projection plus one image block.
func ImageResult(text string, mediaType MediaType, data []byte) ToolResult {
	return ToolResult{
		Text: text,
		Blocks: []ToolResultBlock{
			{Kind: ToolResultBlockText, Text: text},
			{Kind: ToolResultBlockImage, MediaType: mediaType, Data: data},
		},
	}
}

// ToolFunc adapts a plain function into a Tool.
type ToolFunc struct {
	Def ToolDef
	Fn  func(ctx context.Context, args map[string]any) (string, error)
}

func (t *ToolFunc) Definition() ToolDef {
	return t.Def
}

func (t *ToolFunc) Execute(ctx context.Context, args map[string]any) (string, error) {
	return t.Fn(ctx, args)
}

// ToolRegistry holds named tools. It is safe for concurrent use.
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewToolRegistry creates a registry from the given tools.
func NewToolRegistry(tools ...Tool) *ToolRegistry {
	r := &ToolRegistry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		r.tools[t.Definition().Name] = t
	}
	return r
}

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Register adds a tool to the registry.
func (r *ToolRegistry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Definition().Name] = t
}

// Definitions returns all tool definitions.
func (r *ToolRegistry) Definitions() []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition())
	}
	return defs
}

// Execute runs a tool by name.
func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	return t.Execute(ctx, args)
}
