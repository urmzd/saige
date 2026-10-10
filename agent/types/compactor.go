package types

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Compactor reduces message history to fit context windows.
type Compactor interface {
	Compact(ctx context.Context, messages []Message, provider Provider) ([]Message, error)
}

// ── Data-driven compaction config ────────────────────────────────────

// CompactStrategy names a compaction algorithm.
type CompactStrategy string

const (
	CompactNone          CompactStrategy = "none"
	CompactSlidingWindow CompactStrategy = "sliding_window"
	CompactSummarize     CompactStrategy = "summarize"
	// CompactClearToolResults replaces older tool results with a short stub.
	// It needs no model call.
	CompactClearToolResults CompactStrategy = "clear_tool_results"
	// CompactKeepRecent keeps the system prompt, the original task and the
	// last KeepTurns turns, and drops the rest. It needs no model call.
	CompactKeepRecent CompactStrategy = "keep_recent"
	// CompactSummary keeps the system prompt, the original task and the
	// last KeepTurns turns, and replaces the rest with a system-role summary.
	CompactSummary CompactStrategy = "summary"
	// CompactRelevantPlusSummary keeps the last KeepTurns turns and the
	// SelectK older spans most relevant to the latest user turn by BM25,
	// and summarizes the rest.
	CompactRelevantPlusSummary CompactStrategy = "relevant_plus_summary"
	// CompactChain applies the strategies in Chain in order until the
	// history fits TargetTokens.
	CompactChain CompactStrategy = "chain"
)

// CompactConfig is a serialisable description of a compaction strategy.
type CompactConfig struct {
	Strategy   CompactStrategy
	WindowSize int // for sliding_window
	Threshold  int // for summarize: message count excluding a previous summary pair
	KeepLast   int // recent messages to preserve during summarize (default 4)

	// MaxInputTokens switches the trigger from message count to input-token
	// pressure. When set, the loop compacts before a turn whose input would
	// exceed it, measured as the larger of the previous turn's reported
	// prompt tokens and a local estimate of the current history. Summarize
	// (and an empty strategy) then summarize the older half of the branch,
	// with the boundary moved to keep tool calls with their results, instead
	// of applying Threshold and KeepLast. None never makes a summary call.
	// 0 keeps the message-count trigger.
	MaxInputTokens int

	// KeepToolResults is how many of the most recent tool results
	// clear_tool_results leaves intact (default 3).
	KeepToolResults int
	// ExcludeTools names tools whose results clear_tool_results never clears.
	ExcludeTools []string

	// KeepTurns is how many recent turns keep_recent, summary and
	// relevant_plus_summary keep (default 4). A turn is an assistant message
	// with the tool results that answer it and the user messages before it.
	KeepTurns int `json:",omitempty"`
	// SelectK is how many older spans relevant_plus_summary keeps verbatim
	// (default 3).
	SelectK int `json:",omitempty"`
	// SummaryModel names the model that writes summaries, switched on the
	// active provider (for example a cheaper model of the same vendor, or
	// a catalog preset served by a router). Empty uses the active model.
	// AgentConfig.CompactProvider, when set, writes them instead.
	SummaryModel string `json:",omitempty"`
	// Chain lists the strategies a chain applies, in order.
	Chain []CompactConfig `json:",omitempty"`
	// TargetTokens is the input size a chain stops at. Zero uses
	// MaxInputTokens; with neither, a chain applies every step.
	TargetTokens int `json:",omitempty"`
}

// Clone returns a deep copy of cc.
func (cc CompactConfig) Clone() CompactConfig {
	out := cc
	out.ExcludeTools = slices.Clone(cc.ExcludeTools)
	if cc.Chain != nil {
		out.Chain = make([]CompactConfig, len(cc.Chain))
		for i, step := range cc.Chain {
			out.Chain[i] = step.Clone()
		}
	}
	return out
}

// Enabled reports whether cc compacts at all. A nil config and the none
// strategy never compact.
func (cc *CompactConfig) Enabled() bool {
	return cc != nil && cc.Strategy != CompactNone
}

// ToStrategy converts the config into a CompactionStrategy. None, and an
// empty strategy, return nil: the agent summarizes for an empty strategy
// through the tree under input pressure.
func (cc CompactConfig) ToStrategy() CompactionStrategy {
	switch cc.Strategy {
	case CompactKeepRecent:
		return NewKeepRecent(cc.KeepTurns)
	case CompactSummary:
		return &Summary{KeepTurns: cc.KeepTurns, Threshold: cc.Threshold}
	case CompactRelevantPlusSummary:
		return &RelevantPlusSummary{KeepTurns: cc.KeepTurns, K: cc.SelectK, Threshold: cc.Threshold}
	case CompactChain:
		var chain Chain
		for _, step := range cc.Chain {
			if s := step.ToStrategy(); s != nil {
				chain = append(chain, s)
			}
		}
		return chain
	case CompactSlidingWindow, CompactSummarize, CompactClearToolResults:
		return AsStrategy(cc.ToCompactor())
	default:
		return nil
	}
}

// ToCompactor converts the config into a Compactor implementation.
func (cc CompactConfig) ToCompactor() Compactor {
	switch cc.Strategy {
	case CompactSlidingWindow:
		return NewSlidingWindowCompactor(cc.WindowSize)
	case CompactSummarize:
		return NewSummarizeCompactor(cc.Threshold, cc.KeepLast)
	case CompactClearToolResults:
		return NewClearToolResultsCompactor(cc.KeepToolResults, cc.ExcludeTools...)
	case CompactKeepRecent, CompactSummary, CompactRelevantPlusSummary, CompactChain:
		if s, ok := cc.ToStrategy().(Compactor); ok {
			return s
		}
		return NoopCompactor{}
	default:
		return NoopCompactor{}
	}
}

// NoopCompactor passes messages through unchanged.
type NoopCompactor struct{}

func (NoopCompactor) Compact(_ context.Context, messages []Message, _ Provider) ([]Message, error) {
	return messages, nil
}

// SlidingWindowCompactor keeps the first message (system) and the last N messages.
type SlidingWindowCompactor struct {
	WindowSize int
}

func NewSlidingWindowCompactor(n int) *SlidingWindowCompactor {
	return &SlidingWindowCompactor{WindowSize: n}
}

func (c *SlidingWindowCompactor) Compact(_ context.Context, messages []Message, _ Provider) ([]Message, error) {
	if len(messages) <= c.WindowSize+1 {
		return messages, nil
	}
	// Keep first (system) + last N, but don't split a tool-result from its tool-call.
	cut := len(messages) - c.WindowSize
	for cut > 1 && hasToolResult(messages[cut]) {
		cut-- // include the assistant message with the tool call
	}
	if cut <= 0 {
		return messages, nil
	}
	result := make([]Message, 0, len(messages)-cut+1)
	result = append(result, messages[0])
	result = append(result, messages[cut:]...)
	return result, nil
}

// hasToolResult reports whether a message contains a ToolResultPart block.
func hasToolResult(msg Message) bool {
	switch v := msg.(type) {
	case SystemMessage:
		for _, c := range v.Parts {
			if _, ok := c.(ToolResultPart); ok {
				return true
			}
		}
	case UserMessage:
		for _, c := range v.Parts {
			if _, ok := c.(ToolResultPart); ok {
				return true
			}
		}
	}
	return false
}

// SummaryRequestText is the synthetic user turn injected ahead of a summary.
// The summary itself is model-generated, so it lives on an assistant turn;
// this user turn exists because some providers reject histories whose first
// non-system message is from the assistant (no prefill support).
const SummaryRequestText = "Summarize the conversation so far, preserving key facts and decisions."

// SummarizeCompactor summarizes older messages when history exceeds a threshold.
type SummarizeCompactor struct {
	Threshold int
	KeepLast  int
}

func NewSummarizeCompactor(threshold, keepLast int) *SummarizeCompactor {
	if keepLast <= 0 {
		keepLast = 4
	}
	return &SummarizeCompactor{Threshold: threshold, KeepLast: keepLast}
}

func (c *SummarizeCompactor) Compact(ctx context.Context, messages []Message, provider Provider) ([]Message, error) {
	// A previous compaction's summary pair doesn't count toward the threshold:
	// otherwise a compacted history (1 + 2 + KeepLast messages) sits just below
	// the trigger and every subsequent turn re-fires a summarization LLM call.
	effective := len(messages)
	if len(messages) >= 3 && isSummaryPair(messages[1], messages[2]) {
		effective -= 2
	}
	if effective <= c.Threshold {
		return messages, nil
	}

	// The kept suffix never starts with a tool result: it moves back to
	// include the call the result answers.
	keepFrom := len(messages) - min(c.KeepLast, len(messages)-1)
	for keepFrom > 1 && hasToolResult(messages[keepFrom]) {
		keepFrom--
	}
	keepLast := len(messages) - keepFrom

	toSummarize := messages[1:keepFrom]
	// The summary itself costs a user+assistant pair, so summarizing fewer
	// than 3 messages cannot shrink history: skip before paying an LLM call.
	if len(toSummarize) < 3 {
		return messages, nil
	}

	// Build summary prompt
	summaryReq := []Message{
		SystemMsg(Text("Summarize the following conversation concisely, preserving key facts and decisions.")),
		UserMsg(Text(MessagesToText(toSummarize))),
	}

	rx, err := provider.Stream(ctx, Request{Messages: summaryReq})
	if err != nil {
		return messages, nil // fallback: no compaction
	}

	var sb strings.Builder
	var streamErr error
	for delta := range rx {
		switch d := delta.(type) {
		case PartDelta:
			sb.WriteString(d.Text)
		case ErrorDelta:
			// Providers surface mid-stream failures (e.g. rate limits after the
			// stream opened) as ErrorDelta, not as the Stream return error.
			streamErr = d.Error
		}
	}
	summary := sb.String()
	if streamErr != nil || strings.TrimSpace(summary) == "" {
		// A failed or empty summary must not replace real history.
		return messages, nil
	}

	result := make([]Message, 0, keepLast+3)
	result = append(result, messages[0]) // system
	result = append(result, UserMsg(Text(SummaryRequestText)))
	result = append(result, AssistantMsg(Text(summary)))
	result = append(result, messages[len(messages)-keepLast:]...)
	return result, nil
}

// isSummaryPair reports whether a, b are the synthetic summary-request user
// turn and assistant summary produced by a previous compaction.
func isSummaryPair(a, b Message) bool {
	um, ok := a.(UserMessage)
	if !ok || len(um.Parts) != 1 {
		return false
	}
	tc, ok := um.Parts[0].(TextPart)
	if !ok || tc.Text != SummaryRequestText {
		return false
	}
	_, ok = b.(AssistantMessage)
	return ok
}

// MessagesToText converts messages to a plain-text representation, the
// form a summarizer reads. Media never contributes its bytes: each media
// part, in a message or a tool result, is a reference line (see
// MediaReference), so a summary can name what the conversation held and
// where to find it again. Thinking, citations and run metadata are left
// out.
func MessagesToText(msgs []Message) string {
	var b strings.Builder
	line := func(label, text string) {
		b.WriteString(label)
		b.WriteString(text)
		b.WriteByte('\n')
	}
	for _, m := range msgs {
		label := "User: "
		switch m.(type) {
		case SystemMessage:
			label = "System: "
		case AssistantMessage:
			label = "Assistant: "
		}
		for _, p := range PartsOf(m) {
			switch v := p.(type) {
			case TextPart:
				line(label, v.Text)
			case ToolResultPart:
				text := v.Text()
				for _, o := range v.Parts {
					if IsMedia(o) {
						if text != "" {
							text += " "
						}
						text += MediaReference(o)
					}
				}
				line("Tool Result ["+v.CallID+"]: ", text)
			case ToolCallPart:
				line("Tool Call ["+v.ID+"]: ", v.Name)
			case ServerToolCallPart:
				name := v.Name
				if name == "" {
					name = string(v.ToolKind)
				}
				line("Server Tool Call ["+v.ID+"]: ", name)
			case ServerToolResultPart:
				text := v.Text
				for _, o := range v.Outputs {
					if IsMedia(o) {
						if text != "" {
							text += " "
						}
						text += MediaReference(o)
					}
				}
				line("Server Tool Result ["+v.CallID+"]: ", text)
			case RefusalPart:
				line(label, "[refused] "+v.Text)
			case AudioOutPart:
				ref := MediaReference(v)
				if v.Transcript != "" {
					ref += " " + v.Transcript
				}
				line(label, ref)
			default:
				if IsMedia(p) {
					line(label, MediaReference(p))
				}
			}
		}
	}
	return b.String()
}

// mediaDigestChars is how much of a digest a reference line shows: enough
// to tell media apart, short enough to keep the line readable.
const mediaDigestChars = 12

// MediaReference renders a media part as one bracketed line with no bytes:
// its kind and subtype, its size (dimensions, pages or duration), its file
// name, a short digest and its workspace reference or URI, for example
//
//	[image png 1024x768 sha256:9f1c2b7a4e01… saige-artifact://9f1c…]
//
// Inline bytes and vendor file IDs are never included. It returns "" for a
// part that is not media.
func MediaReference(p Part) string {
	src, ok := SourceOf(p)
	if !ok {
		return ""
	}
	fields := []string{string(p.Kind())}
	if mt := string(src.MediaType); mt != "" {
		base, _, _ := strings.Cut(mt, ";")
		if _, sub, found := strings.Cut(base, "/"); found && sub != "" {
			fields = append(fields, strings.TrimSpace(sub))
		} else {
			fields = append(fields, strings.TrimSpace(base))
		}
	}
	dims := func(w, h int) {
		if w > 0 && h > 0 {
			fields = append(fields, fmt.Sprintf("%dx%d", w, h))
		}
	}
	dur := func(d time.Duration) {
		if d > 0 {
			fields = append(fields, d.Round(time.Second/10).String())
		}
	}
	switch v := p.(type) {
	case ImagePart:
		dims(v.Width, v.Height)
	case ImageOutPart:
		dims(v.Width, v.Height)
	case DocumentPart:
		if v.Pages > 0 {
			fields = append(fields, fmt.Sprintf("%d pages", v.Pages))
		}
		if v.Title != "" {
			fields = append(fields, strconv.Quote(v.Title))
		}
	case AudioPart:
		dur(v.Duration)
	case AudioOutPart:
		dur(v.Duration)
	case VideoPart:
		dims(v.Width, v.Height)
		dur(v.Duration)
	case VideoOutPart:
		dims(v.Width, v.Height)
		dur(v.Duration)
	}
	if src.Filename != "" {
		fields = append(fields, strconv.Quote(src.Filename))
	}
	if d := src.Digest; d != "" {
		if len(d) > mediaDigestChars {
			d = d[:mediaDigestChars] + "…"
		}
		fields = append(fields, "sha256:"+d)
	}
	switch {
	case src.Ref != "":
		fields = append(fields, src.Ref)
	case src.URI != "" && !strings.HasPrefix(src.URI, "data:"):
		fields = append(fields, src.URI)
	}
	return "[" + strings.Join(fields, " ") + "]"
}
