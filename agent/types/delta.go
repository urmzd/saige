package types

import (
	"encoding/json"
	"fmt"
	"time"
)

// Delta is a sealed interface for streaming incremental updates.
// Consumers type-switch on concrete delta types to reconstruct state.
type Delta interface {
	isDelta()
}

// ── Part deltas (from LLM) ──────────────────────────────────────────
// A model's output streams as parts. Every part has an Index, its position
// in the final message, unique within one provider attempt. A producer
// sends PartStart, then any number of PartDelta, then PartEnd for each
// index. Deltas for different indices may interleave.

// PartStart opens the part at Index.
type PartStart struct {
	Index int
	Kind  PartKind
	// MediaType is set for media kinds.
	MediaType MediaType
	// ID and Name are set for tool_call and server_tool_call, and ID (the
	// call it answers) for server_tool_result, so a consumer can show a
	// call before its arguments arrive.
	ID, Name string
}

func (PartStart) isDelta() {}

// PartDelta appends to the open part at Index. Exactly one payload field is
// set (see Validate).
type PartDelta struct {
	Index      int
	Text       string // text
	Thinking   string // thinking
	Signature  string // thinking: appended to the signature
	Args       string // tool_call: a JSON fragment of the arguments
	Refusal    string // refusal
	Data       []byte // audio_out, image_out: the next chunk of bytes
	Transcript string // audio_out
}

func (PartDelta) isDelta() {}

// Validate reports a delta whose index is negative or that sets no payload
// field or more than one.
func (d PartDelta) Validate() error {
	if d.Index < 0 {
		return fmt.Errorf("part delta: negative index %d", d.Index)
	}
	n := 0
	for _, set := range []bool{d.Text != "", d.Thinking != "", d.Signature != "", d.Args != "",
		d.Refusal != "", len(d.Data) > 0, d.Transcript != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("part delta %d: %d payload fields set, want 1", d.Index, n)
	}
	return nil
}

// PartEnd closes the part at Index. Part is the complete part: when it is
// set it is authoritative, and when a producer that streamed fragments
// leaves it nil the aggregator builds it from them.
type PartEnd struct {
	Index int
	Part  AssistantPart
}

func (PartEnd) isDelta() {}

// ConversionDelta reports how a provider attempt converted media it could
// not take natively. It is emitted before the attempt's content and kept on
// failover, because a conversion's cost is real.
type ConversionDelta struct {
	Profile string
	Report  ConversionReport
}

func (ConversionDelta) isDelta() {}

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
// Parts carries the full output for consumers, such as TUIs, that render
// media. Citations are the sources the tool attributed its output to.
type ToolExecEndDelta struct {
	ToolCallID string
	Name       string // tool name; empty when the producer does not know it
	Result     string // text projection: UNCHANGED meaning
	Error      string
	Parts      []ToolOutputPart // nil for a result with no parts
	Citations  []Citation
	// Version is the version the tool reported (types.ToolVersion), empty
	// for a tool that reports none.
	Version string
}

func (ToolExecEndDelta) isDelta() {}

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

// CitationDelta carries one attribution a locally executed tool produced
// (through ToolResult.Citations), as the agent's CitationRegistry numbers
// it. Citations the model produces stream as CitationPart parts instead.
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
	// Conversions is the attempt's planned media conversions. Nil when the
	// attempt converted nothing.
	Conversions *ConversionReport
}

func (RouteDelta) isDelta() {}

// ── Run control deltas ───────────────────────────────────────────────

// TruncatedDelta reports that a partial assistant turn was committed to the
// tree with a TruncationPart marker, for example after a stop request or
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

// ── Structured output deltas ─────────────────────────────────────────

// PartialJSONDelta carries the best-effort parse of a structured output that
// is still streaming. JSON is always a complete, valid document: unclosed
// strings, arrays, and objects are closed, so consumers can render it
// directly. Each delta replaces the previous one.
type PartialJSONDelta struct {
	JSON json.RawMessage
}

func (PartialJSONDelta) isDelta() {}
