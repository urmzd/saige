package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// finishRefusal is the finish reason reported for a model refusal.
const finishRefusal = "refusal"

// chatChunkStream is the part of the SDK stream the adapter reads.
type chatChunkStream interface {
	Next() bool
	Current() openai.ChatCompletionChunk
	Err() error
}

// chatCall tracks one streamed tool call by its choice index.
type chatCall struct {
	index    int
	id, name string
	args     strings.Builder
	pending  []string // argument fragments that arrived before the call ID
	started  bool
	ended    bool
}

// openText is a streaming text or refusal part.
type openText struct {
	index int
	buf   strings.Builder
}

// openAudio is a streaming audio_out part.
type openAudio struct {
	index      int
	id         string
	expires    int64
	data       []byte
	transcript strings.Builder
}

// textSpan is a closed text part and its stream index.
type textSpan struct {
	index int
	text  string
}

// chatState is the translation state of one Chat Completions stream. Parts
// get sequential indices in the order they start, which is their order in
// the message.
type chatState struct {
	out         chan<- types.Delta
	emitted     bool
	next        int
	text        *openText
	refusal     *openText
	refusalText string
	audio       *openAudio
	audioMT     types.MediaType
	audioMeta   types.AudioMeta
	calls       map[int64]*chatCall
	order       []int64
	argsFailure streamcheck.ArgsFailure
	// finishReason is read by closeCall.
	finishReason string
	spans        []textSpan
	annotations  []annotation
}

func (s *chatState) emit(d types.Delta) {
	if _, usage := d.(types.UsageDelta); !usage {
		s.emitted = true
	}
	s.out <- d
}

func (s *chatState) alloc() int {
	i := s.next
	s.next++
	return i
}

func (s *chatState) endText() {
	if s.text == nil {
		return
	}
	t := s.text.buf.String()
	s.emit(types.PartEnd{Index: s.text.index, Part: types.TextPart{Text: t}})
	s.spans = append(s.spans, textSpan{s.text.index, t})
	s.text = nil
}

func (s *chatState) addText(delta string) {
	if s.text == nil {
		s.text = &openText{index: s.alloc()}
		s.emit(types.PartStart{Index: s.text.index, Kind: types.KindText})
	}
	s.text.buf.WriteString(delta)
	s.emit(types.PartDelta{Index: s.text.index, Text: delta})
}

func (s *chatState) addRefusal(delta string) {
	if s.refusal == nil {
		s.refusal = &openText{index: s.alloc()}
		s.emit(types.PartStart{Index: s.refusal.index, Kind: types.KindRefusal})
	}
	s.refusal.buf.WriteString(delta)
	s.emit(types.PartDelta{Index: s.refusal.index, Refusal: delta})
}

func (s *chatState) endRefusal() {
	if s.refusal == nil {
		return
	}
	s.refusalText = s.refusal.buf.String()
	s.emit(types.PartEnd{Index: s.refusal.index, Part: types.RefusalPart{Text: s.refusalText}})
	s.refusal = nil
}

func (s *chatState) addAudio(d openai.ChatCompletionChunkChoiceDeltaAudio) error {
	if d.ID == "" && d.Data == "" && d.Transcript == "" && d.ExpiresAt == 0 {
		return nil
	}
	if s.audio == nil {
		s.audio = &openAudio{index: s.alloc()}
		s.emit(types.PartStart{Index: s.audio.index, Kind: types.KindAudioOut, MediaType: s.audioMT})
	}
	a := s.audio
	if d.ID != "" {
		a.id = d.ID
	}
	if d.ExpiresAt != 0 {
		a.expires = d.ExpiresAt
	}
	if d.Data != "" {
		b, err := base64.StdEncoding.DecodeString(d.Data)
		if err != nil {
			return fmt.Errorf("audio delta: %w", err)
		}
		a.data = append(a.data, b...)
		s.emit(types.PartDelta{Index: a.index, Data: b})
	}
	if d.Transcript != "" {
		a.transcript.WriteString(d.Transcript)
		s.emit(types.PartDelta{Index: a.index, Transcript: d.Transcript})
	}
	return nil
}

func (s *chatState) endAudio() {
	a := s.audio
	if a == nil {
		return
	}
	src := types.Source{MediaType: s.audioMT}
	if len(a.data) > 0 {
		src = types.Bytes(s.audioMT, a.data)
	}
	p := types.AudioOutPart{Source: src, AudioMeta: s.audioMeta, Transcript: a.transcript.String(), VendorID: a.id}
	if a.expires > 0 {
		p.ExpiresAt = time.Unix(a.expires, 0).UTC()
	}
	s.emit(types.PartEnd{Index: a.index, Part: p})
	s.audio = nil
}

// closeCall closes c. complete reports that the model finished writing c: a
// later call started, or the choice finished for a reason other than the
// output limit.
func (s *chatState) closeCall(c *chatCall, complete bool) {
	if c.ended || !c.started {
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

func (s *chatState) closeCalls(complete bool) {
	for _, idx := range s.order {
		s.closeCall(s.calls[idx], complete)
	}
}

func (s *chatState) toolCall(tc openai.ChatCompletionChunkChoiceDeltaToolCall) {
	c := s.calls[tc.Index]
	if c == nil {
		c = &chatCall{}
		s.calls[tc.Index] = c
		s.order = append(s.order, tc.Index)
	}
	if c.ended {
		return
	}
	c.name += tc.Function.Name
	if tc.Function.Arguments != "" {
		c.args.WriteString(tc.Function.Arguments)
		if !c.started {
			c.pending = append(c.pending, tc.Function.Arguments)
		}
	}
	if !c.started && tc.ID != "" {
		s.endText()
		// The model writes calls in sequence, so a new call means every
		// earlier one is complete.
		for _, idx := range s.order {
			if idx != tc.Index {
				s.closeCall(s.calls[idx], true)
			}
		}
		c.id, c.started, c.index = tc.ID, true, s.alloc()
		s.emit(types.PartStart{Index: c.index, Kind: types.KindToolCall, ID: c.id, Name: c.name})
		for _, frag := range c.pending {
			s.emit(types.PartDelta{Index: c.index, Args: frag})
		}
		c.pending = nil
		return
	}
	if c.started && tc.Function.Arguments != "" {
		s.emit(types.PartDelta{Index: c.index, Args: tc.Function.Arguments})
	}
}

// finishParts closes every open part at the end of the choice and emits
// the citations of its text.
func (s *chatState) finishParts() {
	s.endText()
	s.endRefusal()
	s.endAudio()
	complete := s.finishReason != "" && !types.IsTruncationFinishReason(s.finishReason) && !types.IsContentFilterFinishReason(s.finishReason)
	s.closeCalls(complete)
	for _, cp := range citationsAcross(s.spans, s.annotations) {
		i := s.alloc()
		s.emit(types.PartStart{Index: i, Kind: types.KindCitation})
		s.emit(types.PartEnd{Index: i, Part: cp})
	}
	s.annotations = nil
}

// consumeStream translates the chunk stream into part deltas.
//
// Text, refusal, audio and each tool call are parts with sequential
// indices. Tool calls are tracked by their choice index. A call closes when
// a later call starts, when the choice finishes, or when the stream ends.
// Its arguments are decoded at close. A call whose JSON does not decode and
// that the model finished writing (a later call started, or the choice
// finished for a reason other than "length") closes with
// ToolCallPart.ArgumentsError set and nil Arguments, so the loop refuses it
// and the model can correct it. The last call of a response stopped at
// "length", or of a stream that ended without a finish reason, is never
// closed: the failure is reported after the stream ends as a truncation or
// an incomplete stream. A structured-output response that stopped at
// "length" is also a truncation, since its JSON is incomplete. A refusal is
// a refusal part, and the stream then ends with a content-filter error.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Adapter) consumeStream(stream chatChunkStream, structured bool) <-chan types.Delta {
	model := string(a.model)
	maxTokens := 0
	if a.params.maxTokens != nil {
		maxTokens = int(*a.params.maxTokens)
	}
	mt, meta := a.audioOutMeta()
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)
		s := &chatState{out: out, calls: map[int64]*chatCall{}, audioMT: mt, audioMeta: meta}

		var responseID, responseModel string
		var outputTokens int
		var failure error

		for failure == nil && stream.Next() {
			chunk := stream.Current()
			if chunk.ID != "" {
				responseID = chunk.ID
			}
			if chunk.Model != "" {
				responseModel = chunk.Model
			}

			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				delta := choice.Delta

				if delta.Content != "" {
					// Text after a call means the model finished writing it.
					s.closeCalls(true)
					s.addText(delta.Content)
				}
				if delta.Refusal != "" {
					s.endText()
					s.addRefusal(delta.Refusal)
				}
				if delta.JSON.Audio.Valid() {
					if err := s.addAudio(delta.Audio); err != nil {
						failure = streamcheck.EventError(providerName, model, delta.Audio.RawJSON(), err)
						break
					}
				}
				if f, ok := delta.JSON.ExtraFields["annotations"]; ok && f.Raw() != "" {
					var anns []annotation
					if json.Unmarshal([]byte(f.Raw()), &anns) == nil {
						s.annotations = append(s.annotations, anns...)
					}
				}
				for _, tc := range delta.ToolCalls {
					s.toolCall(tc)
				}
				if string(choice.FinishReason) != "" {
					s.finishReason = string(choice.FinishReason)
					s.finishParts()
				}
			}

			if chunk.Usage.TotalTokens > 0 {
				outputTokens = int(chunk.Usage.CompletionTokens)
				ud := types.UsageDelta{Cumulative: true,
					PromptTokens:       int(chunk.Usage.PromptTokens),
					CachedPromptTokens: int(chunk.Usage.PromptTokensDetails.CachedTokens),
					CompletionTokens:   int(chunk.Usage.CompletionTokens),
					TotalTokens:        int(chunk.Usage.TotalTokens),
					ResponseID:         responseID,
					ResponseModel:      responseModel,
				}
				ud.PromptByModality, ud.CompletionByModality = chatModalityCounts(chunk.Usage)
				if s.finishReason != "" {
					ud.FinishReasons = []string{s.finishReason}
				}
				s.emit(ud)
			}
		}

		err := stream.Err()
		if failure == nil && err != nil {
			s.endText()
		}
		if failure == nil && err == nil {
			// Close parts left open by a stream that ended without a
			// finish reason, so their decode failures are reported below.
			s.finishParts()
		}
		switch {
		case failure != nil:
			out <- types.ErrorDelta{Error: failure}
		case err != nil:
			out <- types.ErrorDelta{Error: classifyOpenAIError(model, err, !s.emitted)}
		case s.finishReason == "":
			out <- types.ErrorDelta{Error: streamcheck.StreamError(providerName, model, streamcheck.ErrIncompleteStream, !s.emitted)}
		case types.IsContentFilterFinishReason(s.finishReason):
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, model, s.finishReason)}
		case s.refusalText != "":
			out <- types.ErrorDelta{Error: streamcheck.Refused(providerName, model, finishRefusal)}
		case s.argsFailure.Failed():
			out <- types.ErrorDelta{Error: s.argsFailure.Error(providerName, model, s.finishReason, outputTokens, maxTokens)}
		case structured && types.IsTruncationFinishReason(s.finishReason):
			out <- types.ErrorDelta{Error: streamcheck.Truncated(providerName, model, s.finishReason, outputTokens, maxTokens)}
		}
	}()
	return out
}

// chatModalityCounts reads the per-modality token details Chat Completions
// reports. Counts the response leaves at zero are left out; nil when none
// is set.
func chatModalityCounts(u openai.CompletionUsage) (prompt, completion map[types.Modality]int) {
	add := func(m map[types.Modality]int, mo types.Modality, n int64) map[types.Modality]int {
		if n <= 0 {
			return m
		}
		if m == nil {
			m = map[types.Modality]int{}
		}
		m[mo] = int(n)
		return m
	}
	pd, cd := u.PromptTokensDetails, u.CompletionTokensDetails
	prompt = add(prompt, types.ModalityText, pd.TextTokens)
	prompt = add(prompt, types.ModalityImage, pd.ImageTokens)
	prompt = add(prompt, types.ModalityAudio, pd.AudioTokens)
	completion = add(completion, types.ModalityText, cd.TextTokens)
	completion = add(completion, types.ModalityAudio, cd.AudioTokens)
	return prompt, completion
}
