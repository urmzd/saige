package types

import (
	"context"
	"strings"
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
)

// CompactConfig is a serialisable description of a compaction strategy.
type CompactConfig struct {
	Strategy   CompactStrategy
	WindowSize int // for sliding_window
	Threshold  int // for summarize: message count excluding a previous summary pair
	KeepLast   int // recent messages to preserve during summarize (default 4)
}

// ToCompactor converts the config into a Compactor implementation.
func (cc CompactConfig) ToCompactor() Compactor {
	switch cc.Strategy {
	case CompactSlidingWindow:
		return NewSlidingWindowCompactor(cc.WindowSize)
	case CompactSummarize:
		return NewSummarizeCompactor(cc.Threshold, cc.KeepLast)
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
	if cut > 0 && cut < len(messages) && hasToolResult(messages[cut]) {
		cut-- // include the preceding assistant message with the tool call
	}
	if cut <= 0 {
		return messages, nil
	}
	result := make([]Message, 0, len(messages)-cut+1)
	result = append(result, messages[0])
	result = append(result, messages[cut:]...)
	return result, nil
}

// hasToolResult reports whether a message contains a ToolResultContent block.
func hasToolResult(msg Message) bool {
	switch v := msg.(type) {
	case SystemMessage:
		for _, c := range v.Content {
			if _, ok := c.(ToolResultContent); ok {
				return true
			}
		}
	case UserMessage:
		for _, c := range v.Content {
			if _, ok := c.(ToolResultContent); ok {
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

	keepLast := min(c.KeepLast, len(messages)-1)

	toSummarize := messages[1 : len(messages)-keepLast]
	// The summary itself costs a user+assistant pair, so summarizing fewer
	// than 3 messages cannot shrink history: skip before paying an LLM call.
	if len(toSummarize) < 3 {
		return messages, nil
	}

	// Build summary prompt
	summaryReq := []Message{
		NewSystemMessage("Summarize the following conversation concisely, preserving key facts and decisions."),
		NewUserMessage(MessagesToText(toSummarize)),
	}

	rx, err := provider.ChatStream(ctx, summaryReq, nil)
	if err != nil {
		return messages, nil // fallback: no compaction
	}

	var sb strings.Builder
	var streamErr error
	for delta := range rx {
		switch d := delta.(type) {
		case TextContentDelta:
			sb.WriteString(d.Content)
		case ErrorDelta:
			// Providers surface mid-stream failures (e.g. rate limits after the
			// stream opened) as ErrorDelta, not as the ChatStream return error.
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
	result = append(result, NewUserMessage(SummaryRequestText))
	result = append(result, NewAssistantMessage(summary))
	result = append(result, messages[len(messages)-keepLast:]...)
	return result, nil
}

// isSummaryPair reports whether a, b are the synthetic summary-request user
// turn and assistant summary produced by a previous compaction.
func isSummaryPair(a, b Message) bool {
	um, ok := a.(UserMessage)
	if !ok || len(um.Content) != 1 {
		return false
	}
	tc, ok := um.Content[0].(TextContent)
	if !ok || tc.Text != SummaryRequestText {
		return false
	}
	_, ok = b.(AssistantMessage)
	return ok
}

// MessagesToText converts messages to a plain-text representation.
func MessagesToText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch v := m.(type) {
		case SystemMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case TextContent:
					b.WriteString("System: ")
					b.WriteString(bc.Text)
					b.WriteByte('\n')
				case ToolResultContent:
					b.WriteString("Tool Result [")
					b.WriteString(bc.ToolCallID)
					b.WriteString("]: ")
					b.WriteString(bc.Text)
					b.WriteByte('\n')
				}
			}
		case UserMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case TextContent:
					b.WriteString("User: ")
					b.WriteString(bc.Text)
					b.WriteByte('\n')
				case ToolResultContent:
					b.WriteString("Tool Result [")
					b.WriteString(bc.ToolCallID)
					b.WriteString("]: ")
					b.WriteString(bc.Text)
					b.WriteByte('\n')
				case FileContent:
					b.WriteString("User: [file: ")
					b.WriteString(bc.Filename)
					b.WriteString(" (")
					b.WriteString(string(bc.MediaType))
					b.WriteString(")]\n")
				}
			}
		case AssistantMessage:
			for _, c := range v.Content {
				switch bc := c.(type) {
				case TextContent:
					b.WriteString("Assistant: ")
					b.WriteString(bc.Text)
					b.WriteByte('\n')
				case ToolUseContent:
					b.WriteString("Tool Call [")
					b.WriteString(bc.ID)
					b.WriteString("]: ")
					b.WriteString(bc.Name)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}
