package types

import (
	"context"
	"fmt"
	"slices"
)

// ClearToolResultsCompactor replaces the text and blocks of older tool results
// with a short stub naming the tool and call. Tool calls and their results stay
// paired, so the transcript remains valid for every provider. It makes no
// model call, which makes it the cheapest way to recover context in tool-heavy
// runs. The agent loop applies it on a new branch, so the full results remain
// on the original branch of the tree.
type ClearToolResultsCompactor struct {
	Keep    int      // most recent results left intact
	Exclude []string // tools whose results are never cleared
}

// DefaultKeepToolResults is the number of recent tool results kept when
// KeepToolResults is not set.
const DefaultKeepToolResults = 3

// NewClearToolResultsCompactor keeps the newest keep results (default 3) and
// never clears results of the excluded tools.
func NewClearToolResultsCompactor(keep int, exclude ...string) *ClearToolResultsCompactor {
	if keep <= 0 {
		keep = DefaultKeepToolResults
	}
	return &ClearToolResultsCompactor{Keep: keep, Exclude: exclude}
}

// ClearedToolResultText is the stub that replaces a cleared result.
func ClearedToolResultText(tool, callID string) string {
	return fmt.Sprintf("[cleared to save context: result of %s (call %s)]", tool, callID)
}

// Compact returns messages unchanged when nothing needs clearing, so callers
// can detect a no-op by identity.
func (c *ClearToolResultsCompactor) Compact(_ context.Context, messages []Message, _ Provider) ([]Message, error) {
	names := toolCallNames(messages)

	// Positions of clearable results, oldest first.
	type position struct{ msg, block int }
	var clearable []position
	for i, m := range messages {
		for j, r := range toolResults(m) {
			if slices.Contains(c.Exclude, names[r.ToolCallID]) || isClearedStub(r, names[r.ToolCallID]) {
				continue
			}
			clearable = append(clearable, position{i, j})
		}
	}
	if len(clearable) <= c.Keep {
		return messages, nil
	}
	clearable = clearable[:len(clearable)-c.Keep]

	out := slices.Clone(messages)
	for _, p := range clearable {
		out[p.msg] = clearResult(out[p.msg], p.block, names)
	}
	return out, nil
}

// toolCallNames maps each tool call ID in the transcript to its tool name.
func toolCallNames(messages []Message) map[string]string {
	names := map[string]string{}
	for _, m := range messages {
		am, ok := m.(AssistantMessage)
		if !ok {
			continue
		}
		for _, c := range am.Content {
			if tu, ok := c.(ToolUseContent); ok {
				names[tu.ID] = tu.Name
			}
		}
	}
	return names
}

// toolResults returns the tool results of a message in block order.
func toolResults(m Message) []ToolResultContent {
	var out []ToolResultContent
	switch v := m.(type) {
	case SystemMessage:
		for _, c := range v.Content {
			if r, ok := c.(ToolResultContent); ok {
				out = append(out, r)
			}
		}
	case UserMessage:
		for _, c := range v.Content {
			if r, ok := c.(ToolResultContent); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

func isClearedStub(r ToolResultContent, tool string) bool {
	return len(r.Blocks) == 0 && r.Text == ClearedToolResultText(tool, r.ToolCallID)
}

// clearResult returns a copy of m with its nth tool result replaced by a stub.
func clearResult(m Message, nth int, names map[string]string) Message {
	stub := func(r ToolResultContent) ToolResultContent {
		return ToolResultContent{ToolCallID: r.ToolCallID, Text: ClearedToolResultText(names[r.ToolCallID], r.ToolCallID), IsError: r.IsError}
	}
	seen := 0
	switch v := m.(type) {
	case SystemMessage:
		content := slices.Clone(v.Content)
		for i, c := range content {
			if r, ok := c.(ToolResultContent); ok {
				if seen == nth {
					content[i] = stub(r)
				}
				seen++
			}
		}
		return SystemMessage{Content: content}
	case UserMessage:
		content := slices.Clone(v.Content)
		for i, c := range content {
			if r, ok := c.(ToolResultContent); ok {
				if seen == nth {
					content[i] = stub(r)
				}
				seen++
			}
		}
		return UserMessage{Content: content}
	}
	return m
}
