package batch

import (
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Collect drains a provider stream into a batch result: text, thinking and
// tool calls become the assistant message, usage deltas are merged, and the
// first error marks the request errored. The stream is always read to the end.
func Collect(customID string, stream <-chan types.Delta) types.BatchResult {
	res := types.BatchResult{CustomID: customID, Outcome: types.BatchSucceeded}
	var (
		text, thinking strings.Builder
		inText, inTh   bool
		calls          = map[string]*types.ToolUseContent{}
		order          []string
	)
	flushText := func() {
		if inText {
			res.Message.Content = append(res.Message.Content, types.TextContent{Text: text.String()})
			text.Reset()
			inText = false
		}
	}
	for d := range stream {
		switch v := d.(type) {
		case types.TextStartDelta:
			flushText()
			inText = true
		case types.TextContentDelta:
			inText = true
			text.WriteString(v.Content)
		case types.TextEndDelta:
			flushText()
		case types.ThinkingStartDelta:
			inTh = true
			thinking.Reset()
		case types.ThinkingContentDelta:
			inTh = true
			thinking.WriteString(v.Content)
		case types.ThinkingEndDelta:
			if inTh {
				res.Message.Content = append(res.Message.Content, types.ThinkingContent{Thinking: thinking.String(), Signature: v.Signature})
				inTh = false
			}
		case types.ToolCallStartDelta:
			flushText()
			calls[v.ID] = &types.ToolUseContent{ID: v.ID, Name: v.Name}
			order = append(order, v.ID)
		case types.ToolCallEndDelta:
			if c, ok := calls[v.ID]; ok {
				c.Arguments, c.ArgumentsError = v.Arguments, v.ArgumentsError
			}
		case types.ServerToolCallDelta:
			flushText()
			res.Message.Content = append(res.Message.Content, types.ServerToolContent{ID: v.ID, Kind: v.Kind, Name: v.Name, Input: v.Input})
		case types.UsageDelta:
			res.Usage = res.Usage.Merge(v)
			if len(v.FinishReasons) > 0 {
				res.FinishReason = v.FinishReasons[len(v.FinishReasons)-1]
			}
		case types.ErrorDelta:
			if res.Err == nil {
				res.Err = v.Error
			}
		}
	}
	flushText()
	for _, id := range order {
		res.Message.Content = append(res.Message.Content, *calls[id])
	}
	if res.Err != nil {
		res.Outcome = types.BatchErrored
		res.Err = &types.BatchRequestError{Outcome: types.BatchErrored, Message: res.Err.Error(), Err: res.Err}
	}
	return res
}

// Stream replays a batch result as the delta stream a streaming call would
// have produced, so a Coalescer can stand in for a provider. A failed
// request ends with an ErrorDelta carrying its error.
func Stream(r types.BatchResult) <-chan types.Delta {
	out := make(chan types.Delta, 4+3*len(r.Message.Content))
	defer close(out)
	for _, c := range r.Message.Content {
		switch v := c.(type) {
		case types.TextContent:
			out <- types.TextStartDelta{}
			out <- types.TextContentDelta{Content: v.Text}
			out <- types.TextEndDelta{}
		case types.ThinkingContent:
			out <- types.ThinkingStartDelta{}
			out <- types.ThinkingContentDelta{Content: v.Thinking}
			out <- types.ThinkingEndDelta{Signature: v.Signature}
		case types.ToolUseContent:
			out <- types.ToolCallStartDelta{ID: v.ID, Name: v.Name}
			out <- types.ToolCallEndDelta{ID: v.ID, Arguments: v.Arguments, ArgumentsError: v.ArgumentsError}
		case types.ServerToolContent:
			out <- types.ServerToolCallDelta{ID: v.ID, Kind: v.Kind, Name: v.Name, Input: v.Input}
		}
	}
	u := r.Usage
	if r.FinishReason != "" && len(u.FinishReasons) == 0 {
		u.FinishReasons = []string{r.FinishReason}
	}
	if u.PromptTokens > 0 || u.CompletionTokens > 0 || u.TotalTokens > 0 || u.ResponseID != "" || len(u.FinishReasons) > 0 {
		out <- u
	}
	if r.Outcome != types.BatchSucceeded || r.Err != nil {
		err := r.Err
		if err == nil {
			err = &types.BatchRequestError{Outcome: r.Outcome}
		}
		out <- types.ErrorDelta{Error: err}
	}
	return out
}
