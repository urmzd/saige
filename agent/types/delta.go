package types

import "time"

// Delta is a sealed interface for streaming incremental updates.
// Consumers type-switch on concrete delta types to reconstruct state.
type Delta interface {
	isDelta()
}

// ── Text streaming (from LLM) ──────────────────────────────────────

// TextStartDelta signals the beginning of a text block.
type TextStartDelta struct{}

func (TextStartDelta) isDelta() {}

// TextContentDelta carries an incremental text fragment.
type TextContentDelta struct {
	Content string
}

func (TextContentDelta) isDelta() {}

// TextEndDelta signals the end of a text block.
type TextEndDelta struct{}

func (TextEndDelta) isDelta() {}

// ── Tool call streaming (from LLM) ─────────────────────────────────
// These deltas describe what the LLM is generating (its intent to call tools).

// ToolCallStartDelta signals the LLM is generating a tool call.
type ToolCallStartDelta struct {
	ID   string
	Name string
}

func (ToolCallStartDelta) isDelta() {}

// ToolCallArgumentDelta carries a JSON fragment of arguments from the LLM.
type ToolCallArgumentDelta struct {
	Content string
}

func (ToolCallArgumentDelta) isDelta() {}

// ToolCallEndDelta signals the LLM finished generating a tool call.
type ToolCallEndDelta struct {
	Arguments map[string]any
}

func (ToolCallEndDelta) isDelta() {}

// ── Tool execution streaming (from SDK) ─────────────────────────────
// These deltas describe tool execution. Each carries a ToolCallID so
// consumers can demux parallel executions.

// ToolExecStartDelta signals a tool has begun executing.
type ToolExecStartDelta struct {
	ToolCallID string
	Name       string
}

func (ToolExecStartDelta) isDelta() {}

// ToolExecDelta wraps an inner delta from a streaming tool or subagent.
// ToolCallID identifies which parallel execution produced this delta.
type ToolExecDelta struct {
	ToolCallID string
	Inner      Delta
}

func (ToolExecDelta) isDelta() {}

// ToolExecEndDelta signals a tool has finished executing.
type ToolExecEndDelta struct {
	ToolCallID string
	Result     string
	Error      string
}

func (ToolExecEndDelta) isDelta() {}

// ── Thinking streaming (from LLM) ──────────────────────────────────

// ThinkingStartDelta signals the beginning of an extended thinking block.
type ThinkingStartDelta struct{}

func (ThinkingStartDelta) isDelta() {}

// ThinkingContentDelta carries an incremental thinking fragment.
type ThinkingContentDelta struct {
	Content string
}

func (ThinkingContentDelta) isDelta() {}

// ThinkingEndDelta signals the end of an extended thinking block.
// Signature is an opaque token required for multi-turn round-trips
// with providers that support extended thinking (e.g. Anthropic).
type ThinkingEndDelta struct {
	Signature string
}

func (ThinkingEndDelta) isDelta() {}

// ── Marker deltas ───────────────────────────────────────────────────

// MarkerDelta signals that a tool call requires resolution before execution.
// The consumer must call EventStream.ResolveMarker to unblock.
type MarkerDelta struct {
	ToolCallID string
	ToolName   string
	Arguments  map[string]any
	Markers    []Marker
}

func (MarkerDelta) isDelta() {}

// ── Terminal deltas ─────────────────────────────────────────────────

// ErrorDelta carries an error from the stream.
type ErrorDelta struct {
	Error error
}

func (ErrorDelta) isDelta() {}

// DoneDelta signals the stream is complete.
type DoneDelta struct{}

func (DoneDelta) isDelta() {}

// ── Feedback deltas ─────────────────────────────────────────────────

// FeedbackDelta signals that feedback was recorded on a node.
type FeedbackDelta struct {
	TargetNodeID string
	Rating       Rating
	Comment      string
}

func (FeedbackDelta) isDelta() {}

// ── Metadata deltas ──────────────────────────────────────────────────

// UsageDelta carries token usage and latency from an LLM call.
type UsageDelta struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Latency          time.Duration
}

func (UsageDelta) isDelta() {}
