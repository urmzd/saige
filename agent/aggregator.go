package agent

import (
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
type DefaultAggregator struct {
	contentBlocks []types.AssistantContent
	textBuf       strings.Builder
	inText        bool
	toolID        string
	toolName      string
	argsBuf       strings.Builder
	inTool        bool
	thinkingBuf   strings.Builder
	inThinking    bool
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
		a.inTool = true
		a.toolID = v.ID
		a.toolName = v.Name
		a.argsBuf.Reset()
	case types.ToolCallArgumentDelta:
		if a.inTool {
			a.argsBuf.WriteString(v.Content)
		}
	case types.ToolCallEndDelta:
		if a.inTool {
			a.contentBlocks = append(a.contentBlocks, types.ToolUseContent{
				ID:        a.toolID,
				Name:      a.toolName,
				Arguments: v.Arguments,
			})
			a.inTool = false
		}
	}
}

func (a *DefaultAggregator) Message() types.Message {
	// Finalize any in-progress text
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

func (a *DefaultAggregator) Reset() {
	a.contentBlocks = nil
	a.textBuf.Reset()
	a.inText = false
	a.toolID = ""
	a.toolName = ""
	a.argsBuf.Reset()
	a.inTool = false
	a.thinkingBuf.Reset()
	a.inThinking = false
}
