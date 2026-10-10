package types

import (
	"encoding/json"
	"time"
)

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
// ID names the call the fragment belongs to. Producers that interleave
// parallel calls must set it; an empty ID means the most recently started call.
type ToolCallArgumentDelta struct {
	ID      string
	Content string
}

func (ToolCallArgumentDelta) isDelta() {}

// ToolCallEndDelta signals the LLM finished generating a tool call.
// ID names the call being closed, so parallel calls pair with their arguments
// regardless of arrival order. An empty ID closes the oldest open call.
type ToolCallEndDelta struct {
	ID        string
	Arguments map[string]any
	// ArgumentsError is set when the streamed argument text was not valid
	// JSON. Arguments is nil in that case. Producers must never report a parse
	// failure as an empty argument map.
	ArgumentsError string
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
	Name       string // tool name; empty when the producer does not know it
	Result     string // text projection: UNCHANGED meaning
	Error      string
	Blocks     []ToolResultBlock // optional; nil for plain-text results
	// Version is the version the tool reported (types.ToolVersion), empty
	// for a tool that reports none.
	Version string
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
	// Interrupt is the pending decision this marker posted: its ID, the call
	// path from the root run, its kind, and its deadline. A reply can name
	// the interrupt ID instead of the tool call ID. Nil for streams that do
	// not post interrupts, such as remote streams.
	Interrupt *Interrupt
}

func (MarkerDelta) isDelta() {}

// ── Handoff deltas ──────────────────────────────────────────────────

// HandoffDelta signals that control transferred from one agent to another
// mid-stream. The EventStream does not close: subsequent deltas come from the
// new active agent. Consumers use this to re-render headers / attribution.
type HandoffDelta struct {
	From   string // previously active agent; the entry agent reports its own name
	To     string // newly active agent
	Reason string
}

func (HandoffDelta) isDelta() {}

// ── Terminal deltas ─────────────────────────────────────────────────

// CitationDelta carries one attribution as it is produced. Providers that
// return citation metadata (Anthropic document citations, Gemini grounding,
// OpenAI search annotations) emit these from their adapter; locally-executed
// tools emit them via ToolResult.Citations. Either way the consumer sees one
// delta type and the agent's CitationRegistry assigns the number.
//
// The Ordinal is already assigned by the time a consumer sees it, so a UI can
// render the marker immediately without holding its own numbering state.
type CitationDelta struct {
	Citation Citation
	// ToolCallID is set when the citation came from a tool result rather than
	// directly from the model, so a consumer can attribute it to the tool.
	ToolCallID string
}

func (CitationDelta) isDelta() {}

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
	// AccountingID deduplicates budget settlement during durable replay.
	AccountingID string
	// Cumulative marks a provider total snapshot instead of an increment.
	Cumulative bool
	// PromptTokens includes uncached input, cache reads, and cache writes.
	PromptTokens       int
	CachedPromptTokens int
	CacheWriteTokens   int
	CompletionTokens   int
	TotalTokens        int
	Latency            time.Duration

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
	if o.Cumulative {
		u.PromptTokens = max(u.PromptTokens, o.PromptTokens)
		u.CachedPromptTokens = max(u.CachedPromptTokens, o.CachedPromptTokens)
		u.CacheWriteTokens = max(u.CacheWriteTokens, o.CacheWriteTokens)
		u.CompletionTokens = max(u.CompletionTokens, o.CompletionTokens)
	} else {
		u.PromptTokens += o.PromptTokens
		u.CachedPromptTokens += o.CachedPromptTokens
		u.CacheWriteTokens += o.CacheWriteTokens
		u.CompletionTokens += o.CompletionTokens
	}
	if o.AccountingID != "" {
		u.AccountingID = o.AccountingID
	}
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

// RouteDelta identifies the complete configuration used for a provider attempt.
// It does not contain credentials or imply that the attempt succeeded.
type RouteDelta struct {
	Profile  string
	Provider string
	Model    string
	// Experiment and Variant name the traffic split and arm that chose this
	// route; both are empty when no split applied.
	Experiment string
	Variant    string
	// Reason says why this route was chosen, e.g. "primary", "fallback",
	// "canary_demoted". Empty means the first choice.
	Reason string
	// Preset names the declared preset the profile belongs to, ConfigHash
	// identifies the profile's complete configuration, and CatalogRevision
	// the catalog it was resolved from. All are empty for a profile that was
	// not built from a catalog.
	Preset          string
	ConfigHash      string
	CatalogRevision string
	// Options are the effective options of this attempt: the profile's
	// configured options merged with the request's overrides, with every
	// dial compiled. Nil when the profile does not report them.
	Options *RequestOptions
	// Dials reports how the attempt's dials compiled for its model. Nil
	// when the attempt carried no dials.
	Dials *DialReport
}

func (RouteDelta) isDelta() {}

// ── Run control deltas ───────────────────────────────────────────────

// TruncatedDelta reports that a partial assistant turn was committed to the
// tree with a TruncationContent marker, for example after a stop request or
// when the output token limit cut the response short.
type TruncatedDelta struct {
	NodeID string // tree node holding the partial turn; empty if nothing was committed
	Reason string // "interrupted", FinishReasonMaxTokens, or a producer-specific value
}

func (TruncatedDelta) isDelta() {}

// QueuedDelta acknowledges a message submitted while a run is active. The
// message is held until the run reaches a safe point.
type QueuedDelta struct {
	SubmissionID string
	Mode         string // "queue", "steer", "interrupt", "subagent" for a spawned child's result, or "wrap_up"
	Position     int    // 1-based position among pending submissions
}

func (QueuedDelta) isDelta() {}

// InjectedDelta reports that a queued or steering message, or the result of
// a spawned sub-agent, was appended to the conversation and will be seen by
// the next model call.
type InjectedDelta struct {
	SubmissionID string
	Mode         string // "queue", "steer", "interrupt", "subagent", or "wrap_up"
	NodeID       string // tree node the message was appended as
}

func (InjectedDelta) isDelta() {}

// InterruptedDelta reports that the in-flight provider call was stopped on
// request. The run continues with whatever the interrupt asked for.
type InterruptedDelta struct {
	Reason       string
	SubmissionID string // submission that caused the interrupt, if any
}

func (InterruptedDelta) isDelta() {}

// ── Server tool deltas ───────────────────────────────────────────────

// ServerToolCallDelta reports a tool call the provider executes itself (web
// search, code execution, remote MCP). Nothing runs locally, so no gate sees
// it; the delta exists so the call is visible and auditable.
type ServerToolCallDelta struct {
	ID    string
	Kind  ServerToolKind
	Name  string // provider-specific tool name, e.g. "web_search"
	Input map[string]any
}

func (ServerToolCallDelta) isDelta() {}

// ServerToolResultDelta carries the outcome of a ServerToolCallDelta.
type ServerToolResultDelta struct {
	ID      string
	Kind    ServerToolKind
	Text    string          // human-readable projection of the result
	Result  json.RawMessage // provider-native result payload, if any
	IsError bool
	Files   []FileContent // files the tool produced (URIs only on the wire)
}

func (ServerToolResultDelta) isDelta() {}

// ── Structured output deltas ─────────────────────────────────────────

// PartialJSONDelta carries the best-effort parse of a structured output that
// is still streaming. JSON is always a complete, valid document: unclosed
// strings, arrays, and objects are closed, so consumers can render it
// directly. Each delta replaces the previous one.
type PartialJSONDelta struct {
	JSON json.RawMessage
}

func (PartialJSONDelta) isDelta() {}
