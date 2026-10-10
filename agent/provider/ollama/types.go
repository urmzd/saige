package ollama

import (
	"encoding/json"

	"github.com/urmzd/saige/agent/types"
)

// Ollama API wire types.

// ChatMessage is one message of an Ollama chat request or response.
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID and ToolName tie a tool message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
}

// ToolCall is a tool call in an Ollama chat message.
type ToolCall struct {
	// ID is the call ID the server assigns; it is echoed on replay.
	ID       string           `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the function a ToolCall calls and its arguments.
type ToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Tool is a tool definition in an Ollama chat request.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function tool.
type ToolFunction struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  ToolFunctionParams `json:"parameters"`
}

// ToolFunctionParams is a function tool's parameter schema.
type ToolFunctionParams struct {
	Type       string                  `json:"type"`
	Required   []string                `json:"required"`
	Properties map[string]ToolProperty `json:"properties"`
}

// ToolProperty is one property of a ToolFunctionParams schema.
type ToolProperty struct {
	Nullable    bool                    `json:"-"`
	Type        string                  `json:"type"`
	Description string                  `json:"description,omitempty"`
	Enum        []string                `json:"enum,omitempty"`
	Items       *ToolProperty           `json:"items,omitempty"`
	Properties  map[string]ToolProperty `json:"properties,omitempty"`
	Required    []string                `json:"required,omitempty"`
	Default     any                     `json:"default,omitempty"`
}

// ChatRequest is the body of POST /api/chat.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
	Format   any           `json:"format,omitempty"`
	// Options carries generation parameters such as num_ctx, temperature, and
	// num_predict. Omitted entirely when nil, so the daemon's defaults apply.
	Options any `json:"options,omitempty"`
	// Think toggles a reasoning model's thinking phase. Nil leaves the model's
	// own default in place.
	Think *bool `json:"think,omitempty"`
}

// ChatChunk is one line of a streamed /api/chat response.
type ChatChunk struct {
	Model              string      `json:"model,omitempty"`
	Message            ChatMessage `json:"message"`
	Done               bool        `json:"done"`
	DoneReason         string      `json:"done_reason,omitempty"`
	PromptEvalCount    int         `json:"prompt_eval_count,omitempty"`
	EvalCount          int         `json:"eval_count,omitempty"`
	TotalDuration      int64       `json:"total_duration,omitempty"`
	PromptEvalDuration int64       `json:"prompt_eval_duration,omitempty"`
	EvalDuration       int64       `json:"eval_duration,omitempty"`

	// Error is set when the server reports a failure mid-stream, for example
	// when the model runner crashes. It ends the stream.
	Error string `json:"error,omitempty"`
	// Err is set by the client, never by the server: it ends a stream that
	// failed to read, held a malformed line, or went idle.
	Err error `json:"-"`
}

// GenerateRequest is the body of POST /api/generate.
type GenerateRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Stream  bool   `json:"stream"`
	Format  any    `json:"format,omitempty"`
	Options any    `json:"options,omitempty"`
	Think   *bool  `json:"think,omitempty"`
}

// GenerateResponse is a /api/generate response.
type GenerateResponse struct {
	Response string `json:"response"`
	Thinking string `json:"thinking,omitempty"`
	Done     bool   `json:"done"`
	// DoneReason is "length" when num_predict cut the response short.
	DoneReason string `json:"done_reason,omitempty"`
	EvalCount  int    `json:"eval_count,omitempty"`
	Error      string `json:"error,omitempty"`
}

// EmbedRequest is the body of POST /api/embed.
type EmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbedResponse is a /api/embed response.
type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// MarshalJSON preserves the string Type API while emitting nullable type unions.
func (p ToolProperty) MarshalJSON() ([]byte, error) {
	schema := (types.PropertyDef{
		Type: p.Type, Nullable: p.Nullable, Description: p.Description,
		Enum: p.Enum, Required: p.Required, Default: p.Default,
	}).JSONSchema()
	if p.Items != nil {
		schema["items"] = p.Items
	}
	if len(p.Properties) > 0 {
		schema["properties"] = p.Properties
	}
	return json.Marshal(schema)
}
