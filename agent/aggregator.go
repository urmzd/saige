package agent

import (
	"github.com/urmzd/saige/agent/types"
)

// StreamAggregator accumulates deltas into a complete Message.
type StreamAggregator interface {
	Push(delta types.Delta)
	Message() types.Message
	Reset()
}

// DefaultAggregator builds an AssistantMessage from part deltas.
//
// Parts are tracked by index, so deltas for different parts may interleave
// in any order (Start 0, Start 1, Delta 1, Delta 0, End 1, End 0) and the
// message still lists them by index. A delta or end for an index that is not
// open, or a start for a closed one, breaks the protocol: it is counted in
// Violations and ignored. See types.PartAssembler for the rules.
type DefaultAggregator struct {
	asm types.PartAssembler
}

// NewDefaultAggregator creates a new DefaultAggregator.
func NewDefaultAggregator() *DefaultAggregator {
	return &DefaultAggregator{}
}

// Push applies one delta. Deltas other than part deltas are ignored.
func (a *DefaultAggregator) Push(d types.Delta) { a.asm.Push(d) }

// Message returns the turn so far: completed parts and any text part still
// streaming, by index. Open thinking, tool calls and media are not included.
// It returns nil when there is nothing yet.
func (a *DefaultAggregator) Message() types.Message {
	parts := a.asm.Parts()
	if len(parts) == 0 {
		return nil
	}
	return types.AssistantMessage{Parts: parts}
}

// Truncated reports whether a part is still open: text, thinking, a tool
// call whose end has not arrived, or media. A stream that stops in this state
// was cut short rather than finished.
func (a *DefaultAggregator) Truncated() bool { return a.asm.Truncated() }

// OpenToolCalls returns the IDs of tool calls that started but have not
// ended, in index order.
func (a *DefaultAggregator) OpenToolCalls() []string { return a.asm.OpenToolCalls() }

// Violations counts the protocol violations seen since the last Reset.
func (a *DefaultAggregator) Violations() int { return a.asm.Violations() }

// Flush closes the turn for a partial commit, for example after a stop
// request or a max-token cutoff. Open text is kept as a completed text part.
// Open thinking is dropped because providers reject a thinking block without
// its signature, open tool calls because their arguments are incomplete and
// must never be executed, and open media and refusals because they are
// incomplete. truncated reports whether anything was open. After Flush no
// part is open, and Message returns the same committed content.
func (a *DefaultAggregator) Flush() (msg types.Message, truncated bool) {
	msg, truncated, _ = a.FlushDropped()
	return msg, truncated
}

// FlushDropped is Flush that also returns the kinds of the parts it dropped,
// in index order, for the turn's TruncationPart.
func (a *DefaultAggregator) FlushDropped() (msg types.Message, truncated bool, dropped []types.PartKind) {
	parts, truncated, dropped := a.asm.Flush()
	if len(parts) > 0 {
		msg = types.AssistantMessage{Parts: parts}
	}
	return msg, truncated, dropped
}

// Reset empties the aggregator for the next turn.
func (a *DefaultAggregator) Reset() { a.asm.Reset() }
