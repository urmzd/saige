package types

import "encoding/json"

// ── Content role interfaces (sealed) ────────────────────────────────

// SystemContent is content allowed in a SystemMessage.
type SystemContent interface{ isSystemContent() }

// UserContent is content allowed in a UserMessage.
type UserContent interface{ isUserContent() }

// AssistantContent is content allowed in an AssistantMessage.
type AssistantContent interface{ isAssistantContent() }

// ── Media types ─────────────────────────────────────────────────────

// MediaType represents a MIME type for file content.
type MediaType string

const (
	MediaJPEG MediaType = "image/jpeg"
	MediaPNG  MediaType = "image/png"
	MediaGIF  MediaType = "image/gif"
	MediaWebP MediaType = "image/webp"
	MediaPDF  MediaType = "application/pdf"
	MediaCSV  MediaType = "text/csv"
	MediaMP3  MediaType = "audio/mpeg"
	MediaWAV  MediaType = "audio/wav"
	MediaMP4  MediaType = "video/mp4"
	MediaDOCX MediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MediaXLSX MediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MediaPPTX MediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MediaHTML MediaType = "text/html"
	MediaText MediaType = "text/plain"
	MediaJSON MediaType = "application/json"
)

// ── Concrete content blocks ─────────────────────────────────────────

// TextContent holds plain text. Valid in System, User, and Assistant messages.
type TextContent struct {
	Text string
}

func (TextContent) isSystemContent()    {}
func (TextContent) isUserContent()      {}
func (TextContent) isAssistantContent() {}

// ToolUseContent represents a tool invocation by the assistant.
type ToolUseContent struct {
	ID        string
	Name      string
	Arguments map[string]any
	// ArgumentsError is set when the model's argument text could not be
	// decoded. Arguments is then nil, and the agent loop answers the call with
	// an error result instead of running the tool.
	ArgumentsError string
}

func (ToolUseContent) isAssistantContent() {}

// ToolResultContent carries the result of a tool execution.
// Valid in SystemMessage (automatic execution) or UserMessage (human-in-the-loop).
type ToolResultContent struct {
	ToolCallID string
	Text       string // ALWAYS the text projection: providers that ignore Blocks use this
	IsError    bool   // true when Text represents an error, not a successful result
	// Blocks carries optional rich multi-modal output. nil for plain-text results.
	// Blocks with Kind image|file hold bytes in Data (json:"-", not persisted);
	// only the URI/metadata round-trips through tree serialization.
	Blocks []ToolResultBlock `json:"blocks,omitempty"`
	// Citations attributes this result to its sources, carrying the ordinals
	// the run's CitationRegistry assigned. Persisted, so a restored
	// conversation keeps the numbering its earlier answers referenced.
	Citations []Citation `json:"citations,omitempty"`
	// ToolVersion is the version the tool reported when it ran (see
	// ToolVersion), so a transcript names the schema and prompt that
	// produced each result. Empty for a tool that reports none.
	ToolVersion string `json:"tool_version,omitempty"`
}

func (ToolResultContent) isSystemContent() {}
func (ToolResultContent) isUserContent()   {}

// ConfigContent carries agent configuration. Persisted to the tree so
// that serialise/restore round-trips include the full agent config.
// Zero-valued fields mean "no change": only non-zero fields override.
type ConfigContent struct {
	Model      string         // model name passed to Provider (empty = use default)
	MaxIter    int            // max loop iterations (0 = use previous/default)
	Compact    *CompactConfig // compaction strategy (nil = no change)
	CompactNow bool           // trigger immediate compaction this iteration
	// ToolChoice constrains tool use from the next turn on (nil = no change).
	// A required or named choice applies to one model turn and then reverts,
	// so a forced call cannot repeat in a loop. Auto and none stay in effect.
	ToolChoice *ToolChoice `json:",omitempty"`
	// Reason records why this block was written, for example the outcome
	// that made an OutcomePolicy switch models. It has no effect on the loop.
	Reason string `json:",omitempty"`
}

func (ConfigContent) isSystemContent() {}
func (ConfigContent) isUserContent()   {}

// HandoffContent marks a transfer of control to another agent in the same
// handoff group. It is an agent-scoped overlay: it does not mutate the immutable
// root, but selects which agent is active for subsequent iterations. Like
// ConfigContent, it is stripped from the message stream before the LLM sees it:
// its effect is resolved by the agent loop, not sent as text.
type HandoffContent struct {
	To     string `json:"to"`               // target agent name in the group
	From   string `json:"from,omitempty"`   // agent that initiated the handoff
	Reason string `json:"reason,omitempty"` // optional rationale (telemetry / audit only)
	// Message is the handover note the previous owner wrote for the
	// recipient: what it did, what is left, and what to watch for.
	Message string `json:"message,omitempty"`
	// Context carries data the recipient needs that is not in its own view
	// of the conversation, such as identifiers or intermediate results.
	Context string `json:"context,omitempty"`
}

func (HandoffContent) isSystemContent() {}
func (HandoffContent) isUserContent()   {} // allow human-forced handoffs too

// FileContent represents a file attachment. Only valid in UserMessages:
// users attach files, the system/assistant do not.
// Data is tagged json:"-" so tree serialization stores only the URI, not raw bytes.
type FileContent struct {
	URI       string    `json:"uri"`                  // source location (file://, https://, s3://, gs://)
	MediaType MediaType `json:"media_type,omitempty"` // MIME type (inferred from URI or set explicitly)
	Data      []byte    `json:"-"`                    // raw bytes (populated after URI resolution)
	Filename  string    `json:"filename,omitempty"`   // optional display name
}

func (FileContent) isUserContent() {}

// ThinkingContent holds an extended thinking block from a provider that
// supports it (e.g. Anthropic). The Signature is opaque and must be passed
// back to the provider for multi-turn conversations.
type ThinkingContent struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

func (ThinkingContent) isAssistantContent() {}

// CitationContent carries the attributions for the assistant turn it belongs
// to. It is persisted with the message so a restored conversation still knows
// what its claims were sourced from: citations dropped on reload turn a
// grounded answer into an unsourced one.
type CitationContent struct {
	Citations []Citation `json:"citations"`
}

func (CitationContent) isAssistantContent() {}

// ── Feedback ──────────────────────────────────────────────────────────

// Rating represents a binary feedback signal.
type Rating int

const (
	RatingPositive Rating = 1
	RatingNegative Rating = -1
)

// FeedbackContent captures a user's quality rating and optional comment
// on a prior assistant response. Stored in the tree as metadata: stripped
// before messages reach the LLM.
type FeedbackContent struct {
	TargetNodeID string `json:"target_node_id"` // the node being rated (typically an AssistantMessage)
	Rating       Rating `json:"rating"`         // positive or negative
	Comment      string `json:"comment,omitempty"`
}

func (FeedbackContent) isUserContent() {}

// ── Run metadata content ──────────────────────────────────────────────

// ServerToolContent records a tool call the provider executed itself, with
// its input and result, so the transcript and evals can see it. Adapters that
// replay it to the same provider may send it back natively; others drop it.
type ServerToolContent struct {
	ID     string          `json:"id"`
	Kind   ServerToolKind  `json:"kind"`
	Name   string          `json:"name,omitempty"`
	Input  map[string]any  `json:"input,omitempty"`
	Text   string          `json:"text,omitempty"`   // human-readable result projection
	Result json.RawMessage `json:"result,omitempty"` // provider-native result payload
	Files  []FileContent   `json:"files,omitempty"`
}

func (ServerToolContent) isAssistantContent() {}

// SteerContent tags a user message that was injected into an active run
// without cancelling it. The tag is stripped before the provider call; the
// message's other content is sent as normal.
type SteerContent struct {
	ID string `json:"id"` // submission ID
}

func (SteerContent) isUserContent() {}

// TruncationContent marks an assistant turn that was committed before it
// finished. Only completed text is kept: open thinking and open tool calls
// are dropped. It is stripped before the provider call.
type TruncationContent struct {
	Reason string `json:"reason"` // "interrupted", "max_tokens", ...
}

func (TruncationContent) isAssistantContent() {}

// RouteContent records which complete configuration produced the turn it is
// attached to, including any traffic split. It is stripped before the
// provider call.
type RouteContent struct {
	Profile         string          `json:"profile,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	Experiment      string          `json:"experiment,omitempty"`
	Variant         string          `json:"variant,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Preset          string          `json:"preset,omitempty"`
	ConfigHash      string          `json:"config_hash,omitempty"`
	CatalogRevision string          `json:"catalog_revision,omitempty"`
	Options         *RequestOptions `json:"-"`
}

// RouteContentFrom records the configuration a RouteDelta names.
func RouteContentFrom(r RouteDelta) RouteContent {
	c := RouteContent{Profile: r.Profile, Provider: r.Provider, Model: r.Model, Experiment: r.Experiment,
		Variant: r.Variant, Reason: r.Reason, Preset: r.Preset, ConfigHash: r.ConfigHash, CatalogRevision: r.CatalogRevision}
	if r.Options != nil {
		o := r.Options.Clone()
		c.Options = &o
	}
	return c
}

// MarshalJSON writes Options in their snake_case wire form.
func (c RouteContent) MarshalJSON() ([]byte, error) {
	type plain RouteContent
	return json.Marshal(struct {
		plain
		Options *wireOptions `json:"options,omitempty"`
	}{plain(c), toWireOptions(c.Options)})
}

// UnmarshalJSON reads RouteContent, accepting records written before the
// preset fields existed.
func (c *RouteContent) UnmarshalJSON(data []byte) error {
	type plain RouteContent
	var v struct {
		plain
		Options *wireOptions `json:"options,omitempty"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*c = RouteContent(v.plain)
	c.Options = v.Options.requestOptions()
	return nil
}

func (RouteContent) isSystemContent()    {}
func (RouteContent) isAssistantContent() {}

// IsMetadataContent reports whether c is run metadata that the agent loop
// resolves or records itself and never sends to a provider.
func IsMetadataContent(c any) bool {
	switch c.(type) {
	case ConfigContent, HandoffContent, FeedbackContent, SteerContent, TruncationContent, RouteContent, ApprovalContent:
		return true
	default:
		return false
	}
}
