package ollama

// Ollama API wire types.

type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  ToolFunctionParams `json:"parameters"`
}

type ToolFunctionParams struct {
	Type       string                  `json:"type"`
	Required   []string                `json:"required"`
	Properties map[string]ToolProperty `json:"properties"`
}

type ToolProperty struct {
	Type        string                  `json:"type"`
	Description string                  `json:"description,omitempty"`
	Enum        []string                `json:"enum,omitempty"`
	Items       *ToolProperty           `json:"items,omitempty"`
	Properties  map[string]ToolProperty `json:"properties,omitempty"`
	Required    []string                `json:"required,omitempty"`
	Default     any                     `json:"default,omitempty"`
}

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
}

type GenerateRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Stream  bool   `json:"stream"`
	Format  any    `json:"format,omitempty"`
	Options any    `json:"options,omitempty"`
	Think   *bool  `json:"think,omitempty"`
}

type GenerateResponse struct {
	Response string `json:"response"`
	Thinking string `json:"thinking,omitempty"`
	Done     bool   `json:"done"`
}

type EmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}
