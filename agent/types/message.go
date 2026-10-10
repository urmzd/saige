package types

import (
	"iter"
	"strings"
)

// Role represents the sender of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a sealed interface: one of SystemMessage, UserMessage,
// or AssistantMessage. A message is an ordered list of typed parts; the
// role decides which part kinds it may hold.
type Message interface {
	Role() Role
	isMessage()
}

// SystemMessage contains system instructions or automatic tool results.
type SystemMessage struct {
	Parts []SystemPart
}

func (SystemMessage) Role() Role { return RoleSystem }
func (SystemMessage) isMessage() {}

// UserMessage contains user input or human-provided tool results.
type UserMessage struct {
	Parts []UserPart
}

func (UserMessage) Role() Role { return RoleUser }
func (UserMessage) isMessage() {}

// AssistantMessage contains the model's response: text, reasoning, tool
// calls, citations and any media it produced.
type AssistantMessage struct {
	Parts []AssistantPart
}

func (AssistantMessage) Role() Role { return RoleAssistant }
func (AssistantMessage) isMessage() {}

// ── Constructors ─────────────────────────────────────────────────────

// SystemMsg returns a system message holding parts.
func SystemMsg(parts ...SystemPart) SystemMessage { return SystemMessage{Parts: parts} }

// UserMsg returns a user message holding parts.
func UserMsg(parts ...UserPart) UserMessage { return UserMessage{Parts: parts} }

// AssistantMsg returns an assistant message holding parts.
func AssistantMsg(parts ...AssistantPart) AssistantMessage { return AssistantMessage{Parts: parts} }

// ToolResults returns a system message holding tool results. Results from
// automatic execution are system messages: the SDK ran the tools, not the
// user.
func ToolResults(results ...ToolResultPart) SystemMessage {
	parts := make([]SystemPart, len(results))
	for i, r := range results {
		parts[i] = r
	}
	return SystemMessage{Parts: parts}
}

// UserToolResults returns a user message holding tool results, for a human
// in the loop: the agent requested a call and a person answered it.
func UserToolResults(results ...ToolResultPart) UserMessage {
	parts := make([]UserPart, len(results))
	for i, r := range results {
		parts[i] = r
	}
	return UserMessage{Parts: parts}
}

// ── Accessors ────────────────────────────────────────────────────────

// PartsOf returns the parts of m in order. It returns nil for a nil message.
func PartsOf(m Message) []Part {
	switch v := m.(type) {
	case SystemMessage:
		return toParts(v.Parts)
	case *SystemMessage:
		return toParts(v.Parts)
	case UserMessage:
		return toParts(v.Parts)
	case *UserMessage:
		return toParts(v.Parts)
	case AssistantMessage:
		return toParts(v.Parts)
	case *AssistantMessage:
		return toParts(v.Parts)
	default:
		return nil
	}
}

func toParts[P Part](ps []P) []Part {
	if ps == nil {
		return nil
	}
	out := make([]Part, len(ps))
	for i, p := range ps {
		out[i] = p
	}
	return out
}

// TextOf returns the text parts of m concatenated.
func TextOf(m Message) string {
	var b strings.Builder
	for _, t := range Each[TextPart](m) {
		b.WriteString(t.Text)
	}
	return b.String()
}

// Each iterates over the parts of m that have type T, yielding each part's
// position in the message.
func Each[T Part](m Message) iter.Seq2[int, T] {
	return func(yield func(int, T) bool) {
		for i, p := range PartsOf(m) {
			if v, ok := p.(T); ok {
				if !yield(i, v) {
					return
				}
			}
		}
	}
}
