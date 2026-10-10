package openai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3/responses"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// Responses API stream event and item types the adapter reads.
const (
	eventOutputItemAdded    = "response.output_item.added"
	eventOutputItemDone     = "response.output_item.done"
	eventContentPartAdded   = "response.content_part.added"
	eventContentPartDone    = "response.content_part.done"
	eventArgumentsDelta     = "response.function_call_arguments.delta"
	eventArgumentsDone      = "response.function_call_arguments.done"
	eventOutputTextDelta    = "response.output_text.delta"
	eventOutputTextDone     = "response.output_text.done"
	eventAnnotationAdded    = "response.output_text.annotation.added"
	eventRefusalDelta       = "response.refusal.delta"
	eventRefusalDone        = "response.refusal.done"
	eventSummaryTextDelta   = "response.reasoning_summary_text.delta"
	eventReasoningTextDelta = "response.reasoning_text.delta"
	eventCompleted          = "response.completed"
	eventIncomplete         = "response.incomplete"
	eventFailed             = "response.failed"
	eventError              = "error"
	itemFunctionCall        = "function_call"
	itemReasoning           = "reasoning"
	itemMessage             = "message"
	contentOutputText       = "output_text"
	contentRefusal          = "refusal"
	finishStop              = "stop"
	finishLength            = "length"
	incompleteOutputTokens  = "max_output_tokens"
)

// responsesStream is the part of the SDK stream the adapter reads.
type responsesStream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
}

// responsesCall tracks one streamed function call by its output item ID.
type responsesCall struct {
	index    int
	id, name string
	args     strings.Builder
	ended    bool
}

// contentKey names a content part of an output item.
type contentKey struct{ output, content int64 }

// responsesText is a streaming output_text or refusal content part.
type responsesText struct {
	index   int
	refusal bool
	buf     strings.Builder
	anns    []annotation
}

// responsesReasoning is a streaming reasoning item. Its part starts with
// the first summary or reasoning text, or at the item's end when only
// encrypted content arrives.
type responsesReasoning struct {
	index       int
	started     bool
	summary     bool
	lastSummary int64
	buf         strings.Builder
}

// responsesState is the translation state of one streamed response. Parts
// get indices in the order they start, which follows the output items'
// output_index and their content_index.
type responsesState struct {
	out         chan<- types.Delta
	emitted     bool
	next        int
	texts       map[contentKey]*responsesText
	reasoning   map[int64]*responsesReasoning
	calls       map[string]*responsesCall
	order       []string
	argsFailure streamcheck.ArgsFailure
	refused     bool
	// finish is the normalized finish reason: stop, length, or the
	// incomplete reason the API reported. Empty means the response did not
	// reach a terminal event.
	finish       string
	outputTokens int
	failure      error
}

func newResponsesState(out chan<- types.Delta) *responsesState {
	return &responsesState{out: out, texts: map[contentKey]*responsesText{},
		reasoning: map[int64]*responsesReasoning{}, calls: map[string]*responsesCall{}}
}

func (s *responsesState) emit(d types.Delta) {
	if _, usage := d.(types.UsageDelta); !usage {
		s.emitted = true
	}
	s.out <- d
}

func (s *responsesState) alloc() int {
	i := s.next
	s.next++
	return i
}

// text returns the content part at k, starting it when needed.
func (s *responsesState) text(k contentKey, refusal bool) *responsesText {
	if t := s.texts[k]; t != nil {
		return t
	}
	t := &responsesText{index: s.alloc(), refusal: refusal}
	s.texts[k] = t
	kind := types.KindText
	if refusal {
		kind = types.KindRefusal
	}
	s.emit(types.PartStart{Index: t.index, Kind: kind})
	return t
}

// endText closes the content part at k with final as its text when the
// event carries it, then emits its citations.
func (s *responsesState) endText(k contentKey, final string, hasFinal bool) {
	t := s.texts[k]
	if t == nil {
		return
	}
	delete(s.texts, k)
	text := t.buf.String()
	if hasFinal {
		text = final
	}
	if t.refusal {
		s.refused = true
		s.emit(types.PartEnd{Index: t.index, Part: types.RefusalPart{Text: text}})
		return
	}
	s.emit(types.PartEnd{Index: t.index, Part: types.TextPart{Text: text}})
	for _, cp := range citationsAcross([]textSpan{{t.index, text}}, t.anns) {
		i := s.alloc()
		s.emit(types.PartStart{Index: i, Kind: types.KindCitation})
		s.emit(types.PartEnd{Index: i, Part: cp})
	}
}

// endItemTexts closes every open content part of output item o, or of every
// item when o is negative, in index order.
func (s *responsesState) endItemTexts(o int64) { s.endTexts(o, true) }

// endTexts is endItemTexts; refusals closes refusal parts too.
func (s *responsesState) endTexts(o int64, refusals bool) {
	keys := make([]contentKey, 0, len(s.texts))
	for k, t := range s.texts {
		if (o < 0 || k.output == o) && (refusals || !t.refusal) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return s.texts[keys[i]].index < s.texts[keys[j]].index })
	for _, k := range keys {
		s.endText(k, "", false)
	}
}

func (s *responsesState) thinking(o int64) *responsesReasoning {
	r := s.reasoning[o]
	if r == nil {
		r = &responsesReasoning{lastSummary: -1}
		s.reasoning[o] = r
	}
	if !r.started {
		r.started, r.index = true, s.alloc()
		s.emit(types.PartStart{Index: r.index, Kind: types.KindThinking})
	}
	return r
}

func (s *responsesState) thinkingText(o int64, summaryIndex int64, summary bool, delta string) {
	if delta == "" {
		return
	}
	r := s.thinking(o)
	if summary {
		r.summary = true
		if r.lastSummary >= 0 && summaryIndex != r.lastSummary {
			r.buf.WriteString("\n\n")
			s.emit(types.PartDelta{Index: r.index, Thinking: "\n\n"})
		}
		r.lastSummary = summaryIndex
	}
	r.buf.WriteString(delta)
	s.emit(types.PartDelta{Index: r.index, Thinking: delta})
}

// endReasoning closes the reasoning item at output index o. item is the
// finished item, which carries the encrypted content and, when no summary
// streamed, the summary text.
func (s *responsesState) endReasoning(o int64, item *responses.ResponseOutputItemUnion) {
	r := s.reasoning[o]
	var enc string
	var summaries []string
	if item != nil {
		enc = item.EncryptedContent
		for _, sm := range item.Summary {
			if sm.Text != "" {
				summaries = append(summaries, sm.Text)
			}
		}
	}
	if (r == nil || !r.started) && enc == "" && len(summaries) == 0 {
		delete(s.reasoning, o)
		return
	}
	r = s.thinking(o)
	if r.buf.Len() == 0 && len(summaries) > 0 {
		t := strings.Join(summaries, "\n\n")
		r.summary = true
		r.buf.WriteString(t)
		s.emit(types.PartDelta{Index: r.index, Thinking: t})
	}
	if enc != "" {
		s.emit(types.PartDelta{Index: r.index, Signature: enc})
	}
	s.emit(types.PartEnd{Index: r.index, Part: types.ThinkingPart{Text: r.buf.String(), Signature: enc, Summary: r.summary}})
	delete(s.reasoning, o)
}

func (s *responsesState) endAllReasoning() {
	outs := make([]int64, 0, len(s.reasoning))
	for o := range s.reasoning {
		outs = append(outs, o)
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i] < outs[j] })
	for _, o := range outs {
		s.endReasoning(o, nil)
	}
}

// closeCall closes c. complete reports that the model finished writing it.
func (s *responsesState) closeCall(c *responsesCall, complete bool) {
	if c == nil || c.ended {
		return
	}
	c.ended = true
	args, err := streamcheck.DecodeArguments(c.args.String())
	switch {
	case err == nil:
		s.emit(types.PartEnd{Index: c.index, Part: types.ToolCallPart{ID: c.id, Name: c.name, Arguments: args}})
	case complete:
		s.emit(types.PartEnd{Index: c.index, Part: types.ToolCallPart{ID: c.id, Name: c.name, ArgumentsError: err.Error()}})
	default:
		s.argsFailure.Set(c.id, c.name, err)
	}
}

func (s *responsesState) closeAll(complete bool) {
	for _, id := range s.order {
		s.closeCall(s.calls[id], complete)
	}
}

// annotationOf decodes a streamed annotation.
func annotationOf(raw string) (annotation, bool) {
	var a annotation
	if raw == "" || json.Unmarshal([]byte(raw), &a) != nil {
		return a, false
	}
	return a, true
}

// handle translates one stream event.
//
//nolint:gocyclo // one switch over the event types; splitting it would scatter shared state
func (s *responsesState) handle(model string, ev responses.ResponseStreamEventUnion) {
	k := contentKey{ev.OutputIndex, ev.ContentIndex}
	switch ev.Type {
	case eventContentPartAdded:
		switch ev.Part.Type {
		case contentOutputText:
			s.text(k, false)
		case contentRefusal:
			s.text(k, true)
		}
	case eventOutputTextDelta:
		if ev.Delta != "" {
			t := s.text(k, false)
			t.buf.WriteString(ev.Delta)
			s.emit(types.PartDelta{Index: t.index, Text: ev.Delta})
		}
	case eventAnnotationAdded:
		if a, ok := annotationOf(ev.Annotation.RawJSON()); ok {
			s.text(k, false).anns = append(s.text(k, false).anns, a)
		}
	case eventOutputTextDone:
		s.endText(k, ev.Text, ev.JSON.Text.Valid())
	case eventRefusalDelta:
		if ev.Delta != "" {
			t := s.text(k, true)
			t.buf.WriteString(ev.Delta)
			s.emit(types.PartDelta{Index: t.index, Refusal: ev.Delta})
		}
	case eventRefusalDone:
		s.endText(k, ev.Refusal, ev.JSON.Refusal.Valid())
	case eventContentPartDone:
		if t := s.texts[k]; t != nil {
			if !t.refusal && len(t.anns) == 0 {
				for _, ra := range ev.Part.Annotations {
					if a, ok := annotationOf(ra.RawJSON()); ok {
						t.anns = append(t.anns, a)
					}
				}
			}
			if t.refusal {
				s.endText(k, ev.Part.Refusal, ev.Part.JSON.Refusal.Valid())
			} else {
				s.endText(k, ev.Part.Text, ev.Part.JSON.Text.Valid())
			}
		}
	case eventSummaryTextDelta:
		s.thinkingText(ev.OutputIndex, ev.SummaryIndex, true, ev.Delta)
	case eventReasoningTextDelta:
		s.thinkingText(ev.OutputIndex, 0, false, ev.Delta)
	case eventOutputItemAdded:
		if ev.Item.Type != itemFunctionCall {
			return
		}
		// The model writes items in sequence, so a new call means every
		// earlier one is complete.
		s.endItemTexts(-1)
		s.closeAll(true)
		c := &responsesCall{index: s.alloc(), id: ev.Item.CallID, name: ev.Item.Name}
		s.calls[ev.Item.ID] = c
		s.order = append(s.order, ev.Item.ID)
		s.emit(types.PartStart{Index: c.index, Kind: types.KindToolCall, ID: c.id, Name: c.name})
	case eventArgumentsDelta:
		if c := s.calls[ev.ItemID]; c != nil && !c.ended && ev.Delta != "" {
			c.args.WriteString(ev.Delta)
			s.emit(types.PartDelta{Index: c.index, Args: ev.Delta})
		}
	case eventArgumentsDone:
		if c := s.calls[ev.ItemID]; c != nil && !c.ended {
			if c.args.Len() == 0 && ev.Arguments != "" {
				c.args.WriteString(ev.Arguments)
			}
			s.closeCall(c, true)
		}
	case eventOutputItemDone:
		switch ev.Item.Type {
		case itemFunctionCall:
			s.closeCall(s.calls[ev.Item.ID], true)
		case itemReasoning:
			item := ev.Item
			s.endReasoning(ev.OutputIndex, &item)
		case itemMessage:
			s.endItemTexts(ev.OutputIndex)
		}
	case eventCompleted, eventIncomplete:
		s.finish = finishStop
		if ev.Type == eventIncomplete {
			s.finish = ev.Response.IncompleteDetails.Reason
			if s.finish == incompleteOutputTokens || s.finish == "" {
				s.finish = finishLength
			}
		}
		s.endAllReasoning()
		s.endItemTexts(-1)
		// A call cut off by the output limit or the safety system is not
		// complete.
		s.closeAll(!types.IsTruncationFinishReason(s.finish) && !types.IsContentFilterFinishReason(s.finish))
		s.usage(ev.Response)
	case eventFailed:
		s.failure = streamcheck.EventError(providerName, model, ev.Response.Error.RawJSON(),
			fmt.Errorf("response failed: %s", ev.Response.Error.Message))
	case eventError:
		s.failure = streamcheck.EventError(providerName, model, ev.RawJSON(),
			fmt.Errorf("stream error: %s", ev.Message))
	}
}

func (s *responsesState) usage(resp responses.Response) {
	u := resp.Usage
	s.outputTokens = int(u.OutputTokens)
	if u.TotalTokens == 0 {
		return
	}
	s.emit(types.UsageDelta{Cumulative: true,
		PromptTokens:       int(u.InputTokens),
		CachedPromptTokens: int(u.InputTokensDetails.CachedTokens),
		CompletionTokens:   int(u.OutputTokens),
		TotalTokens:        int(u.TotalTokens),
		ResponseID:         resp.ID,
		ResponseModel:      string(resp.Model),
		FinishReasons:      []string{s.finish},
	})
}

// consume translates the event stream into part deltas. Failure handling
// matches the Chat Completions adapter: a call the model finished writing
// whose arguments do not decode closes with ArgumentsError, a call cut off
// by the output limit is never closed and the turn fails as truncated, a
// refusal is a refusal part followed by a content-filter error, and a
// stream that ends without a terminal event fails as incomplete.
func (r *ResponsesAdapter) consume(stream responsesStream, structured bool) <-chan types.Delta {
	model := string(r.base.model)
	maxTokens := 0
	if r.base.params.maxTokens != nil {
		maxTokens = int(*r.base.params.maxTokens)
	}
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)
		s := newResponsesState(out)
		for s.failure == nil && stream.Next() {
			s.handle(model, stream.Current())
		}
		err := stream.Err()
		if s.failure == nil {
			// Text that streamed before a cut is kept as it arrived.
			s.endTexts(-1, false)
		}
		switch {
		case s.failure != nil:
			out <- types.ErrorDelta{Error: s.failure}
		case err != nil:
			out <- types.ErrorDelta{Error: classifyOpenAIError(model, err, !s.emitted)}
		case s.finish == "":
			out <- types.ErrorDelta{Error: streamcheck.StreamError(providerName, model, streamcheck.ErrIncompleteStream, !s.emitted)}
		case types.IsContentFilterFinishReason(s.finish):
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, model, s.finish)}
		case s.refused:
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, model, finishRefusal)}
		case s.argsFailure.Failed():
			out <- types.ErrorDelta{Error: s.argsFailure.Error(providerName, model, s.finish, s.outputTokens, maxTokens)}
		case structured && types.IsTruncationFinishReason(s.finish):
			out <- types.ErrorDelta{Error: streamcheck.Truncated(providerName, model, s.finish, s.outputTokens, maxTokens)}
		}
	}()
	return out
}
