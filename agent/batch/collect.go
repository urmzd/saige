package batch

import "github.com/urmzd/saige/agent/types"

// Collect drains a provider stream into a batch result: the part deltas
// become the assistant message, usage deltas are merged, and the first error
// marks the request errored. The stream is always read to the end.
func Collect(customID string, stream <-chan types.Delta) types.BatchResult {
	res := types.BatchResult{CustomID: customID, Outcome: types.BatchSucceeded}
	asm := types.NewPartAssembler()
	for d := range stream {
		switch v := d.(type) {
		case types.PartStart, types.PartDelta, types.PartEnd:
			asm.Push(v)
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
	parts, _, _ := asm.Flush()
	res.Message.Parts = parts
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
	deltas := types.MessageDeltas(r.Message)
	out := make(chan types.Delta, 2+len(deltas))
	defer close(out)
	for _, d := range deltas {
		out <- d
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
