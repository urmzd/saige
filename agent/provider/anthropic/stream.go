package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// streamBlock is one content block while it streams.
type streamBlock struct {
	typ, name, id string
	// pos is the block's position among the parts this stream emits.
	pos        int
	args       []byte
	textLen    int
	structured bool
	citations  []types.Citation
	// part is the complete part for blocks that arrive whole (redacted
	// thinking, server tool results); it is sent on the block's stop.
	part types.AssistantPart
}

// consumeStream reads the Anthropic stream and emits part deltas. Each
// content block is the part at its own content block index. Citations on a
// text block, and a refusal, are parts after the last block: they are sent
// when the stop reason arrives, with indices that follow the blocks, so the
// index is still the part's position. A citation's anchor covers its whole
// text block, which is how the API splits cited text.
//
// With structured set, the hidden schema tool's input streams as text.
//
// A tool call whose argument JSON does not decode is held open, because
// Anthropic sends content_block_stop before the message_delta that carries
// stop_reason. When a later block starts, or the stop reason is not
// max_tokens, the model finished writing the call: its PartEnd carries a
// ToolCallPart with ArgumentsError set and nil Arguments, so the loop
// refuses it and the model can correct it. A max_tokens stop, or a stream
// that ends before any stop reason, never closes the call and reports a
// truncation or an incomplete stream instead. Malformed structured output is
// always an error. Errors arrive after the usage delta, so the consumer can
// still account for the tokens.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Adapter) consumeStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], structured bool) <-chan types.Delta {
	out := make(chan types.Delta, 64)
	model := string(a.model)
	maxTokens := int(a.maxTokens)
	go func() {
		defer close(out)

		blocks := map[int]*streamBlock{}
		// next is the first index after every block seen; parts emitted
		// at the stop take indices from it.
		next, parts := 0, 0
		// serverKinds remembers each server tool call's kind, so its result
		// block, which carries only the call ID, reports the same kind.
		serverKinds := map[string]types.ServerToolKind{}
		var cited []types.CitationPart
		closed := false

		var responseID, responseModel, finishReason string
		var outputTokens int
		stopped := false

		// emitted turns true once a content delta reaches the consumer; after
		// that a transport error can no longer be retried.
		emitted := false
		var argsFailure streamcheck.ArgsFailure
		emit := func(d types.Delta) {
			if _, usage := d.(types.UsageDelta); !usage {
				emitted = true
			}
			out <- d
		}
		// whole emits a part that arrives complete, at the next index.
		whole := func(p types.AssistantPart) {
			emit(types.PartStart{Index: next, Kind: p.Kind()})
			emit(types.PartEnd{Index: next, Part: p})
			next++
		}
		// held is a tool call whose arguments did not decode, waiting to
		// learn whether the model finished writing it.
		var held *streamcheck.ArgsFailure
		heldIndex := -1
		releaseHeld := func(complete bool) {
			if held == nil {
				return
			}
			if complete {
				emit(types.PartEnd{Index: heldIndex, Part: types.ToolCallPart{ID: held.ToolCallID, Name: held.Name, ArgumentsError: held.Err.Error()}})
			} else {
				argsFailure.Set(held.ToolCallID, held.Name, held.Err)
			}
			held, heldIndex = nil, -1
		}
		// closeTurn sends the parts that follow the blocks: citations, then
		// a refusal.
		closeTurn := func(refusal *anthropic.RefusalStopDetails) {
			if closed {
				return
			}
			closed = true
			for _, c := range cited {
				whole(c)
			}
			if refusal != nil {
				whole(refusalPart(*refusal))
			}
		}

		for stream.Next() {
			evt := stream.Current()
			idx := int(evt.Index)

			switch evt.Type {
			case "message_start":
				responseID = evt.Message.ID
				responseModel = string(evt.Message.Model)
				u := evt.Message.Usage
				if prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens; prompt > 0 {
					emit(types.UsageDelta{Cumulative: true,
						CompletionTokens:   int(u.OutputTokens),
						PromptTokens:       int(prompt),
						CachedPromptTokens: int(u.CacheReadInputTokens),
						CacheWriteTokens:   int(u.CacheCreationInputTokens),
						TotalTokens:        int(prompt + u.OutputTokens),
						ResponseID:         evt.Message.ID,
						ResponseModel:      string(evt.Message.Model),
					})
				}

			case "content_block_start":
				releaseHeld(true)
				cb := evt.ContentBlock
				b := &streamBlock{typ: cb.Type, name: cb.Name, id: cb.ID, pos: parts}
				start := types.PartStart{Index: idx}
				switch {
				case cb.Type == blockText:
					start.Kind = types.KindText
				case cb.Type == blockThinking:
					start.Kind = types.KindThinking
				case cb.Type == blockRedactedThinking:
					start.Kind = types.KindThinking
					b.part = types.ThinkingPart{Redacted: true, Signature: cb.Data}
				case cb.Type == blockToolUse && structured && cb.Name == structuredToolName:
					b.structured = true
					start.Kind = types.KindText
				case cb.Type == blockToolUse:
					start.Kind, start.ID, start.Name = types.KindToolCall, cb.ID, cb.Name
				case cb.Type == blockServerToolUse:
					serverKinds[cb.ID] = serverToolKind(cb.Name)
					start.Kind, start.ID, start.Name = types.KindServerToolCall, cb.ID, cb.Name
				case isServerToolResult(cb.Type) && cb.ToolUseID != "":
					// Server tool results arrive whole in the start event.
					start.Kind, start.ID = types.KindServerToolResult, cb.ToolUseID
					b.part = a.serverToolResult(cb.RawJSON(), serverResultKind(cb.Type, cb.ToolUseID, serverKinds))
				default:
					// A block type this adapter does not map carries no part.
					continue
				}
				blocks[idx] = b
				next = max(next, idx+1)
				parts++
				emit(start)
				if b.typ == blockText && cb.Text != "" {
					b.textLen += len(cb.Text)
					emit(types.PartDelta{Index: idx, Text: cb.Text})
				}

			case "content_block_delta":
				b := blocks[idx]
				if b == nil {
					continue
				}
				switch evt.Delta.Type {
				case "text_delta":
					if evt.Delta.Text != "" {
						b.textLen += len(evt.Delta.Text)
						emit(types.PartDelta{Index: idx, Text: evt.Delta.Text})
					}
				case "citations_delta":
					if c, ok := citationFrom(evt.Delta.Citation.RawJSON()); ok {
						b.citations = append(b.citations, c)
					}
				case "thinking_delta":
					if evt.Delta.Thinking != "" {
						emit(types.PartDelta{Index: idx, Thinking: evt.Delta.Thinking})
					}
				case "signature_delta":
					if evt.Delta.Signature != "" {
						emit(types.PartDelta{Index: idx, Signature: evt.Delta.Signature})
					}
				case "input_json_delta":
					b.args = append(b.args, evt.Delta.PartialJSON...)
					switch {
					case evt.Delta.PartialJSON == "":
					case b.structured:
						emit(types.PartDelta{Index: idx, Text: evt.Delta.PartialJSON})
					default:
						emit(types.PartDelta{Index: idx, Args: evt.Delta.PartialJSON})
					}
				}

			case "content_block_stop":
				b := blocks[idx]
				if b == nil {
					continue
				}
				delete(blocks, idx)
				switch {
				case b.part != nil:
					emit(types.PartEnd{Index: idx, Part: b.part})
				case b.structured:
					if _, err := streamcheck.DecodeArguments(string(b.args)); err != nil {
						argsFailure.Set("", "", err)
					}
					emit(types.PartEnd{Index: idx})
				case b.typ == blockToolUse:
					args, err := streamcheck.DecodeArguments(string(b.args))
					if err != nil {
						held, heldIndex = &streamcheck.ArgsFailure{ToolCallID: b.id, Name: b.name, Err: err}, idx
						continue
					}
					emit(types.PartEnd{Index: idx, Part: types.ToolCallPart{ID: b.id, Name: b.name, Arguments: args}})
				case b.typ == blockServerToolUse:
					// Input that does not decode is reported as absent; the
					// provider ran the call, so there is nothing to refuse.
					input, _ := streamcheck.DecodeArguments(string(b.args))
					emit(types.PartEnd{Index: idx, Part: types.ServerToolCallPart{ID: b.id, ToolKind: serverKinds[b.id],
						Name: b.name, Input: input}})
				default:
					emit(types.PartEnd{Index: idx})
					for _, c := range b.citations {
						cited = append(cited, anchored(c, b.pos, b.textLen))
					}
				}

			case "message_stop":
				stopped = true
				closeTurn(nil)

			case "message_delta":
				if string(evt.Delta.StopReason) != "" {
					finishReason = string(evt.Delta.StopReason)
					releaseHeld(!types.IsTruncationFinishReason(finishReason) && !types.IsContentFilterFinishReason(finishReason) &&
						finishReason != stopContextWindowExceeded)
					var refusal *anthropic.RefusalStopDetails
					if finishReason == stopRefusal {
						d := evt.Delta.StopDetails
						refusal = &d
					}
					closeTurn(refusal)
				}
				if evt.Usage.OutputTokens > 0 {
					outputTokens = int(evt.Usage.OutputTokens)
					ud := types.UsageDelta{Cumulative: true,
						CompletionTokens: int(evt.Usage.OutputTokens),
						TotalTokens:      int(evt.Usage.OutputTokens),
						ResponseID:       responseID,
						ResponseModel:    responseModel,
					}
					if finishReason != "" {
						ud.FinishReasons = []string{finishReason}
					}
					emit(ud)
				}
			}
		}

		releaseHeld(false)
		switch err := stream.Err(); {
		case err != nil:
			out <- types.ErrorDelta{Error: classifyAnthropicError(model, err, !emitted)}
		case !stopped && finishReason == "":
			// A clean close without message_stop or stop_reason means the
			// connection ended early; reporting success would hand the loop a
			// partial answer.
			out <- types.ErrorDelta{Error: streamcheck.StreamError("anthropic", model, streamcheck.ErrIncompleteStream, !emitted)}
		case types.IsContentFilterFinishReason(finishReason):
			out <- types.ErrorDelta{Error: streamcheck.Refused("anthropic", model, finishReason)}
		case finishReason == stopContextWindowExceeded:
			// The answer is partial; reporting it as the context limit lets
			// the loop compact or fail instead of taking it as final.
			out <- types.ErrorDelta{Error: &types.ProviderError{Provider: "anthropic", Model: model,
				Kind: types.ErrorKindContextLength, Err: errContextWindowExceeded}}
		case finishReason == stopPauseTurn:
			// The API paused a long server tool turn and expects the partial
			// response sent back to continue it. The loop does not resume a
			// paused turn, so report it rather than hand the loop a partial
			// answer as final.
			out <- types.ErrorDelta{Error: &types.ProviderError{Provider: "anthropic", Model: model,
				Kind: types.ErrorKindPermanent, Err: errPausedTurn}}
		case argsFailure.Failed():
			out <- types.ErrorDelta{Error: argsFailure.Error("anthropic", model, finishReason, outputTokens, maxTokens)}
		}
	}()

	return out
}
