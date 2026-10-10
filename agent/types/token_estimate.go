package types

import (
	"context"
	"encoding/json"
)

// Rough constants for EstimateTokens. They are deliberately simple: the
// estimate only decides when to compact, and the provider's reported prompt
// tokens take over after the first turn.
const (
	estimateCharsPerToken   = 4
	estimateMessageOverhead = 4    // role and framing tokens per message
	estimateFileTokens      = 1000 // an attached file or rich block of unknown size
)

// EstimateTokens approximates the input tokens of messages at four characters
// per token. It covers text, thinking, tool arguments and tool results, and
// charges a flat amount per file or rich block.
func EstimateTokens(messages []Message) int {
	chars, fixed := 0, 0
	for _, m := range messages {
		fixed += estimateMessageOverhead
		switch v := m.(type) {
		case SystemMessage:
			for _, c := range v.Parts {
				c, f := estimateContent(c)
				chars, fixed = chars+c, fixed+f
			}
		case UserMessage:
			for _, c := range v.Parts {
				c, f := estimateContent(c)
				chars, fixed = chars+c, fixed+f
			}
		case AssistantMessage:
			for _, c := range v.Parts {
				c, f := estimateContent(c)
				chars, fixed = chars+c, fixed+f
			}
		}
	}
	return fixed + (chars+estimateCharsPerToken-1)/estimateCharsPerToken
}

func estimateContent(c any) (chars, fixed int) {
	switch v := c.(type) {
	case TextPart:
		return len(v.Text), 0
	case ThinkingPart:
		return len(v.Text), 0
	case ToolCallPart:
		args, _ := json.Marshal(v.Arguments)
		return len(v.Name) + len(args), 0
	case ToolResultPart:
		media := 0
		for _, p := range v.Parts {
			if IsMedia(p) {
				media++
			}
		}
		return len(v.Text()), media * estimateFileTokens
	case Part:
		if IsMedia(v) {
			return 0, estimateFileTokens
		}
	}
	return 0, 0
}

// EstimatingTokenizer is a Tokenizer backed by EstimateTokens. It needs no
// network call and is the default when no provider tokenizer is configured.
type EstimatingTokenizer struct{}

// CountTokens returns EstimateTokens(messages).
func (EstimatingTokenizer) CountTokens(_ context.Context, messages []Message) (int, error) {
	return EstimateTokens(messages), nil
}
