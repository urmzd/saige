package anthropic

import (
	"context"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// streamBlock is one content block while it streams.
type streamBlock struct {
	typ, name, id string
	// index is the part index the block streams at.
	index      int
	args       []byte
	textLen    int
	structured bool
	citations  []types.Citation
	// part is the complete part for blocks that arrive whole (redacted
	// thinking, server tool results); it is sent on the block's stop.
	part types.AssistantPart
}

// maxPauseContinuations bounds how many times one turn is continued after
// the API paused it, so a server tool loop that never finishes still ends.
const maxPauseContinuations = 8

// turnUsage is the token usage of one response, or of the responses of a
// continued turn added up.
type turnUsage struct {
	prompt, cached, written, output int
}

func (u turnUsage) plus(o turnUsage) turnUsage {
	return turnUsage{u.prompt + o.prompt, u.cached + o.cached, u.written + o.written, u.output + o.output}
}

// consumeStream sends params and emits the response as part deltas. Parts
// are numbered in the order their content blocks start, so the index is the
// part's position in the turn. The citations of a text block are parts of
// their own, sent as soon as the block stops, with the indices after it: an
// attempt that dies later in the stream keeps them. A citation's anchor
// covers its whole text block, which is how the API splits cited text. A
// refusal is a part after the last block, sent when the stop reason
// arrives.
//
// A turn the API pauses during a long server tool loop (stop reason
// pause_turn) is continued: the partial response is sent back as the
// assistant turn, exactly as received, and the next response streams on as
// the same turn, its parts numbered after the ones already sent. Usage is
// reported for the whole turn, every request added up, with Requests set to
// their count. After maxPauseContinuations, or when the partial response
// cannot be replayed, the pause ends the stream with an error.
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
func (a *Adapter) consumeStream(ctx context.Context, params anthropic.MessageNewParams, structured bool) <-chan types.Delta {
	out := make(chan types.Delta, 64)
	model := string(a.model)
	maxTokens := int(a.maxTokens)
	go func() {
		defer close(out)

		// next is the index of the next part the turn emits.
		next := 0
		// serverKinds remembers each server tool call's kind, so its result
		// block, which carries only the call ID, reports the same kind. A
		// continued turn's result may answer a call of an earlier response.
		serverKinds := map[string]types.ServerToolKind{}
		// done is the usage of the responses before the current one.
		var done turnUsage
		requests := 1

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

		stream := a.client.Messages.NewStreaming(ctx, params)
		var responseID, responseModel, finishReason string
		var cur turnUsage
		var stopped bool
		for {
			// blocks maps the content block indices of this response to
			// their parts.
			blocks := map[int]*streamBlock{}
			closed := false
			responseID, responseModel, finishReason, stopped = "", "", "", false
			cur = turnUsage{}
			// acc keeps the response as the API sent it, to replay it when
			// the API pauses the turn.
			var acc anthropic.Message
			var accErr error
			usage := func() types.UsageDelta {
				t := done.plus(cur)
				u := types.UsageDelta{Cumulative: true,
					PromptTokens: t.prompt, CachedPromptTokens: t.cached, CacheWriteTokens: t.written,
					CompletionTokens: t.output, TotalTokens: t.prompt + t.output,
					ResponseID: responseID, ResponseModel: responseModel,
				}
				if requests > 1 {
					u.Requests = requests
				}
				return u
			}
			// closeTurn sends the part that follows the blocks: a refusal.
			closeTurn := func(refusal *anthropic.RefusalStopDetails) {
				if closed {
					return
				}
				closed = true
				if refusal != nil {
					whole(refusalPart(*refusal))
				}
			}

			for stream.Next() {
				evt := stream.Current()
				if accErr == nil {
					accErr = acc.Accumulate(evt)
				}
				idx := int(evt.Index)

				switch evt.Type {
				case "message_start":
					responseID = evt.Message.ID
					responseModel = string(evt.Message.Model)
					u := evt.Message.Usage
					cur = turnUsage{prompt: int(u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens),
						cached: int(u.CacheReadInputTokens), written: int(u.CacheCreationInputTokens), output: int(u.OutputTokens)}
					if cur.prompt > 0 {
						emit(usage())
					}

				case "content_block_start":
					releaseHeld(true)
					cb := evt.ContentBlock
					b := &streamBlock{typ: cb.Type, name: cb.Name, id: cb.ID, index: next}
					start := types.PartStart{Index: next}
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
					next++
					emit(start)
					if b.typ == blockText && cb.Text != "" {
						b.textLen += len(cb.Text)
						emit(types.PartDelta{Index: b.index, Text: cb.Text})
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
							emit(types.PartDelta{Index: b.index, Text: evt.Delta.Text})
						}
					case "citations_delta":
						if c, ok := citationFrom(evt.Delta.Citation.RawJSON()); ok {
							b.citations = append(b.citations, c)
						}
					case "thinking_delta":
						if evt.Delta.Thinking != "" {
							emit(types.PartDelta{Index: b.index, Thinking: evt.Delta.Thinking})
						}
					case "signature_delta":
						if evt.Delta.Signature != "" {
							emit(types.PartDelta{Index: b.index, Signature: evt.Delta.Signature})
						}
					case "input_json_delta":
						b.args = append(b.args, evt.Delta.PartialJSON...)
						switch {
						case evt.Delta.PartialJSON == "":
						case b.structured:
							emit(types.PartDelta{Index: b.index, Text: evt.Delta.PartialJSON})
						default:
							emit(types.PartDelta{Index: b.index, Args: evt.Delta.PartialJSON})
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
						emit(types.PartEnd{Index: b.index, Part: b.part})
					case b.structured:
						if _, err := streamcheck.DecodeArguments(string(b.args)); err != nil {
							argsFailure.Set("", "", err)
						}
						emit(types.PartEnd{Index: b.index})
					case b.typ == blockToolUse:
						args, err := streamcheck.DecodeArguments(string(b.args))
						if err != nil {
							held, heldIndex = &streamcheck.ArgsFailure{ToolCallID: b.id, Name: b.name, Err: err}, b.index
							continue
						}
						emit(types.PartEnd{Index: b.index, Part: types.ToolCallPart{ID: b.id, Name: b.name, Arguments: args}})
					case b.typ == blockServerToolUse:
						// Input that does not decode is reported as absent; the
						// provider ran the call, so there is nothing to refuse.
						input, _ := streamcheck.DecodeArguments(string(b.args))
						emit(types.PartEnd{Index: b.index, Part: types.ServerToolCallPart{ID: b.id, ToolKind: serverKinds[b.id],
							Name: b.name, Input: input}})
					default:
						emit(types.PartEnd{Index: b.index})
						for _, c := range b.citations {
							whole(anchored(c, b.index, b.textLen))
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
						cur.output = int(evt.Usage.OutputTokens)
						ud := usage()
						if finishReason != "" {
							ud.FinishReasons = []string{finishReason}
						}
						emit(ud)
					}
				}
			}

			if stream.Err() != nil || finishReason != stopPauseTurn || accErr != nil || requests > maxPauseContinuations {
				break
			}
			// Continue the paused turn: send the response back as it was
			// received and stream the rest of the turn.
			params.Messages = continuePaused(params.Messages, acc.ToParam())
			done = done.plus(cur)
			requests++
			stream = a.client.Messages.NewStreaming(ctx, params)
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
			// The turn stayed paused after every continuation, or its
			// partial response could not be replayed: report it rather than
			// hand the loop a partial answer as final.
			out <- types.ErrorDelta{Error: &types.ProviderError{Provider: "anthropic", Model: model,
				Kind: types.ErrorKindPermanent, Err: errPausedTurn}}
		case argsFailure.Failed():
			out <- types.ErrorDelta{Error: argsFailure.Error("anthropic", model, finishReason, cur.output, maxTokens)}
		}
	}()

	return out
}

// continuePaused returns the messages that continue a paused turn: msgs
// with the partial response as the final assistant turn. A request that
// already ended with an assistant turn (a prefill) is continued by adding
// the response's blocks to that turn, since the response continued it.
func continuePaused(msgs []anthropic.MessageParam, partial anthropic.MessageParam) []anthropic.MessageParam {
	msgs = slices.Clone(msgs)
	if n := len(msgs); n > 0 && msgs[n-1].Role == anthropic.MessageParamRoleAssistant {
		last := msgs[n-1]
		last.Content = append(slices.Clone(last.Content), partial.Content...)
		msgs[n-1] = last
		return msgs
	}
	partial.Role = anthropic.MessageParamRoleAssistant
	return append(msgs, partial)
}
