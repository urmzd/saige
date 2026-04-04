package types

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
}

func (ToolUseContent) isAssistantContent() {}

// ToolResultContent carries the result of a tool execution.
// Valid in SystemMessage (automatic execution) or UserMessage (human-in-the-loop).
type ToolResultContent struct {
	ToolCallID string
	Text       string
	IsError    bool // true when Text represents an error, not a successful result
}

func (ToolResultContent) isSystemContent() {}
func (ToolResultContent) isUserContent()   {}

// ConfigContent carries agent configuration. Persisted to the tree so
// that serialise/restore round-trips include the full agent config.
// Zero-valued fields mean "no change" — only non-zero fields override.
type ConfigContent struct {
	Model      string         // model name passed to Provider (empty = use default)
	MaxIter    int            // max loop iterations (0 = use previous/default)
	Compact    *CompactConfig // compaction strategy (nil = no change)
	CompactNow bool           // trigger immediate compaction this iteration
}

func (ConfigContent) isSystemContent() {}
func (ConfigContent) isUserContent()   {}

// FileContent represents a file attachment. Only valid in UserMessages —
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

// ── Feedback ──────────────────────────────────────────────────────────

// Rating represents a binary feedback signal.
type Rating int

const (
	RatingPositive Rating = 1
	RatingNegative Rating = -1
)

// FeedbackContent captures a user's quality rating and optional comment
// on a prior assistant response. Stored in the tree as metadata — stripped
// before messages reach the LLM.
type FeedbackContent struct {
	TargetNodeID string `json:"target_node_id"` // the node being rated (typically an AssistantMessage)
	Rating       Rating `json:"rating"`         // positive or negative
	Comment      string `json:"comment,omitempty"`
}

func (FeedbackContent) isUserContent() {}
