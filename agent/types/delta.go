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
// Result is the text projection shown to humans (unchanged contract).
// Blocks carries optional rich output for consumers (e.g. TUIs) that render images.
type ToolExecEndDelta struct {
	ToolCallID string
	Result     string            // text projection — UNCHANGED meaning
	Error      string
	Blocks     []ToolResultBlock // optional; nil for plain-text results
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

// ── Handoff deltas ──────────────────────────────────────────────────

// HandoffDelta signals that control transferred from one agent to another
// mid-stream. The EventStream does not close — subsequent deltas come from the
// new active agent. Consumers use this to re-render headers / attribution.
type HandoffDelta struct {
	From   string // previously active agent ("" if entry agent)
	To     string // newly active agent
	Reason string
}

func (HandoffDelta) isDelta() {}

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

	// Response metadata for OpenTelemetry GenAI semantic conventions.
	ResponseModel string   // gen_ai.response.model
	ResponseID    string   // gen_ai.response.id
	FinishReasons []string // gen_ai.response.finish_reasons

	// CacheHit is true when this usage was served from a response cache.
	// Token fields carry the ORIGINAL recorded counts for observability, but
	// cost/billing accounting should treat a cache hit as zero new tokens.
	CacheHit bool
}

func (UsageDelta) isDelta() {}

// Merge combines two usage deltas, accumulating token counts and taking the
// most recent non-zero metadata. Providers may emit usage in multiple parts
// (e.g. Anthropic reports prompt tokens at message_start and completion tokens
// at message_delta); Merge reassembles the full total. Latency is taken from
// the most recent non-zero value, not summed.
func (u UsageDelta) Merge(o UsageDelta) UsageDelta {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	if o.Latency != 0 {
		u.Latency = o.Latency
	}
	if o.ResponseModel != "" {
		u.ResponseModel = o.ResponseModel
	}
	if o.ResponseID != "" {
		u.ResponseID = o.ResponseID
	}
	if len(o.FinishReasons) > 0 {
		u.FinishReasons = o.FinishReasons
	}
	if o.CacheHit {
		u.CacheHit = true
	}
	return u
}
