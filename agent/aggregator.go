package agent

import (
	"encoding/json"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// StreamAggregator accumulates deltas into a complete Message.
type StreamAggregator interface {
	Push(delta types.Delta)
	Message() types.Message
	Reset()
}

// DefaultAggregator builds an AssistantMessage from streaming deltas.
//
// Tool calls are tracked per ID, so parallel calls pair with their own
// arguments even when a provider interleaves them (Start a, Start b, End a,
// End b). An End without an ID closes the oldest open call, which matches
// providers that announce the next call before finishing the previous one.
type DefaultAggregator struct {
	contentBlocks []types.AssistantContent
	textBuf       strings.Builder
	inText        bool
	thinkingBuf   strings.Builder
	inThinking    bool
	// open holds started tool calls that have not ended, in start order.
	open []*pendingToolCall
}

// pendingToolCall is a tool call whose arguments are still streaming. The
// End delta normally carries the parsed arguments. The raw fragments are kept
// so an End without arguments can still be decoded, and so text that is not
// valid JSON is reported instead of becoming an empty argument map.
type pendingToolCall struct {
	id   string
	name string
	args strings.Builder
}

// NewDefaultAggregator creates a new DefaultAggregator.
func NewDefaultAggregator() *DefaultAggregator {
	return &DefaultAggregator{}
}

func (a *DefaultAggregator) Push(d types.Delta) {
	switch v := d.(type) {
	case types.ThinkingStartDelta:
		a.inThinking = true
		a.thinkingBuf.Reset()
	case types.ThinkingContentDelta:
		if a.inThinking {
			a.thinkingBuf.WriteString(v.Content)
		}
	case types.ThinkingEndDelta:
		if a.inThinking {
			a.contentBlocks = append(a.contentBlocks, types.ThinkingContent{
				Thinking:  a.thinkingBuf.String(),
				Signature: v.Signature,
			})
			a.inThinking = false
		}
	case types.TextStartDelta:
		a.inText = true
		a.textBuf.Reset()
	case types.TextContentDelta:
		if a.inText {
			a.textBuf.WriteString(v.Content)
		}
	case types.TextEndDelta:
		if a.inText {
			a.contentBlocks = append(a.contentBlocks, types.TextContent{Text: a.textBuf.String()})
			a.inText = false
		}
	case types.ToolCallStartDelta:
		// A repeated Start for an open ID restarts that call rather than
		// opening a duplicate.
		if v.ID != "" {
			if i := a.openIndex(v.ID); i >= 0 {
				a.open = append(a.open[:i], a.open[i+1:]...)
			}
		}
		a.open = append(a.open, &pendingToolCall{id: v.ID, name: v.Name})
	case types.ToolCallArgumentDelta:
		if tc := a.argumentTarget(v.ID); tc != nil {
			tc.args.WriteString(v.Content)
		}
	case types.ToolCallEndDelta:
		i := a.endTarget(v.ID)
		if i < 0 {
			return
		}
		tc := a.open[i]
		a.open = append(a.open[:i], a.open[i+1:]...)
		args, argsErr := v.Arguments, v.ArgumentsError
		if args == nil && argsErr == "" {
			args, argsErr = decodeToolArguments(tc.args.String())
		}
		if argsErr != "" {
			args = nil
		}
		a.contentBlocks = append(a.contentBlocks, types.ToolUseContent{
			ID:             tc.id,
			Name:           tc.name,
			Arguments:      args,
			ArgumentsError: argsErr,
		})
	case types.ServerToolCallDelta:
		a.contentBlocks = append(a.contentBlocks, types.ServerToolContent{
			ID: v.ID, Kind: v.Kind, Name: v.Name, Input: v.Input,
		})
	case types.ServerToolResultDelta:
		// A result fills in its call. A result whose call was not streamed
		// still records what the provider ran.
		for i := len(a.contentBlocks) - 1; i >= 0; i-- {
			if st, ok := a.contentBlocks[i].(types.ServerToolContent); ok && st.ID == v.ID {
				st.Text, st.Result, st.Files = v.Text, v.Result, v.Files
				if st.Kind == "" {
					st.Kind = v.Kind
				}
				a.contentBlocks[i] = st
				return
			}
		}
		a.contentBlocks = append(a.contentBlocks, types.ServerToolContent{
			ID: v.ID, Kind: v.Kind, Text: v.Text, Result: v.Result, Files: v.Files,
		})
	}
}

// argumentTarget picks the call an argument fragment belongs to: the call
// with the given ID, or the most recently started call when the fragment has
// no ID. Arguments always follow their own Start, so the newest open call is
// the one still receiving text.
func (a *DefaultAggregator) argumentTarget(id string) *pendingToolCall {
	if id != "" {
		if i := a.openIndex(id); i >= 0 {
			return a.open[i]
		}
		return nil
	}
	if len(a.open) == 0 {
		return nil
	}
	return a.open[len(a.open)-1]
}

// decodeToolArguments parses buffered argument text. Empty text means a call
// with no arguments. Text that is not a JSON object yields an error message,
// never a nil map that would look like a valid empty call.
func decodeToolArguments(raw string) (map[string]any, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err.Error()
	}
	return args, ""
}

// openIndex returns the position of the open call with id, or -1.
func (a *DefaultAggregator) openIndex(id string) int {
	for i, tc := range a.open {
		if tc.id == id {
			return i
		}
	}
	return -1
}

// endTarget picks the call an End closes: the call with the given ID, or the
// oldest open call when the End has no ID. It returns -1 when nothing matches.
func (a *DefaultAggregator) endTarget(id string) int {
	if id != "" {
		return a.openIndex(id)
	}
	if len(a.open) == 0 {
		return -1
	}
	return 0
}

// Message returns the turn so far. Completed blocks are included, and so is
// any text block still streaming. Open thinking and open tool calls are not.
func (a *DefaultAggregator) Message() types.Message {
	blocks := make([]types.AssistantContent, len(a.contentBlocks))
	copy(blocks, a.contentBlocks)

	if a.inText && a.textBuf.Len() > 0 {
		blocks = append(blocks, types.TextContent{Text: a.textBuf.String()})
	}

	if len(blocks) == 0 {
		return nil
	}
	return types.AssistantMessage{Content: blocks}
}

// Truncated reports whether a block is still open: text, thinking, or a tool
// call whose End has not arrived. A stream that stops in this state was cut
// short rather than finished.
func (a *DefaultAggregator) Truncated() bool {
	return a.inText || a.inThinking || len(a.open) > 0
}

// OpenToolCalls returns the IDs of tool calls that started but have not
// ended, in start order.
func (a *DefaultAggregator) OpenToolCalls() []string {
	if len(a.open) == 0 {
		return nil
	}
	ids := make([]string, len(a.open))
	for i, tc := range a.open {
		ids[i] = tc.id
	}
	return ids
}

// Flush closes the turn for a partial commit, for example after a stop
// request or a max-token cutoff. Open text is kept as a completed text block.
// Open thinking is dropped because providers reject a thinking block without
// its signature, and open tool calls are dropped because their arguments are
// incomplete and must never be executed. truncated reports whether anything
// was open. After Flush no block is open, and Message returns the same
// committed content.
func (a *DefaultAggregator) Flush() (msg types.Message, truncated bool) {
	truncated = a.Truncated()
	if a.inText {
		if a.textBuf.Len() > 0 {
			a.contentBlocks = append(a.contentBlocks, types.TextContent{Text: a.textBuf.String()})
		}
		a.inText = false
		a.textBuf.Reset()
	}
	a.inThinking = false
	a.thinkingBuf.Reset()
	a.open = nil
	return a.Message(), truncated
}

func (a *DefaultAggregator) Reset() {
	a.contentBlocks = nil
	a.textBuf.Reset()
	a.inText = false
	a.open = nil
	a.thinkingBuf.Reset()
	a.inThinking = false
}
