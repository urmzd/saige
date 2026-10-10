package google

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// textSpan is one text part of the turn and where its text starts in the
// turn's text, so grounding offsets can be anchored to it.
type textSpan struct {
	index  int
	offset int
	text   strings.Builder
}

// responseMapper turns the parts of one candidate into part deltas, for a
// stream (one call per chunk) or a complete response (one call). Indices
// count up from 0 in emission order, so each is the part's position in the
// finished message.
//
// Text, thinking and audio output are runs: consecutive chunks of the same
// kind extend one part, so a consumer sees one part per run rather than one
// per network chunk. Every other part is whole when it arrives and is
// started and ended at once.
type responseMapper struct {
	emit func(types.Delta)
	next int

	// open is the index of the open run, or -1.
	open     int
	openKind types.PartKind
	// runSig is the thought signature of the open run. A thinking run sends
	// it as its signature; a text or audio run is followed by an empty,
	// signed thinking part (see mapper.modelParts for the replay).
	runSig []byte
	// audio holds the open audio run's bytes.
	audio      []byte
	audioType  string
	spans      []*textSpan
	textLength int

	grounding *genai.GroundingMetadata
	server    serverToolState
	// started turns true at the first part, after which a failed stream
	// can no longer be retried.
	started bool
}

func newResponseMapper(emit func(types.Delta)) *responseMapper {
	return &responseMapper{emit: emit, open: -1}
}

// candidate maps the parts of one chunk of a candidate and keeps its
// grounding metadata for finish.
func (r *responseMapper) candidate(c *genai.Candidate) error {
	if c == nil {
		return nil
	}
	if c.Content != nil {
		for _, p := range c.Content.Parts {
			if p == nil {
				continue
			}
			if err := r.part(p); err != nil {
				return err
			}
		}
	}
	if md := c.GroundingMetadata; md != nil && (len(md.GroundingChunks) > 0 || len(md.GroundingSupports) > 0 || len(md.WebSearchQueries) > 0) {
		r.grounding = md
	}
	return nil
}

func (r *responseMapper) part(p *genai.Part) error {
	sig := p.ThoughtSignature
	switch {
	case p.Thought && p.Text != "":
		r.run(types.KindThinking, "")
		r.emit(types.PartDelta{Index: r.open, Thinking: p.Text})
		if len(sig) > 0 {
			r.runSig = sig
		}
	case p.Text != "":
		r.run(types.KindText, "")
		span := r.spans[len(r.spans)-1]
		span.text.WriteString(p.Text)
		r.textLength += len(p.Text)
		r.emit(types.PartDelta{Index: r.open, Text: p.Text})
		if len(sig) > 0 {
			r.runSig = sig
		}
	case p.FunctionCall != nil:
		r.closeRun()
		if len(sig) > 0 {
			// Gemini 3 signs the function call part itself and rejects a
			// later request that returns the call without it.
			r.signature(sig)
		}
		r.toolCall(p.FunctionCall)
	case p.ExecutableCode != nil:
		r.closeRun()
		c := r.server.codeCall(p.ExecutableCode)
		r.whole(types.PartStart{Kind: types.KindServerToolCall, ID: c.ID, Name: c.Name}, c)
		r.signature(sig)
	case p.CodeExecutionResult != nil:
		r.closeRun()
		res := r.server.codeResult(p.CodeExecutionResult)
		r.whole(types.PartStart{Kind: types.KindServerToolResult, ID: res.CallID}, res)
		r.signature(sig)
	case p.InlineData != nil:
		return r.media(p.InlineData.MIMEType, p.InlineData.Data, "", sig)
	case p.FileData != nil:
		return r.media(p.FileData.MIMEType, nil, p.FileData.FileURI, sig)
	case len(sig) > 0:
		// A signature on an otherwise empty part belongs to the run it
		// closes, or stands alone.
		if r.open >= 0 {
			r.runSig = sig
		} else {
			r.signature(sig)
		}
	}
	return nil
}

// start opens a part at the next index.
func (r *responseMapper) start(s types.PartStart) int {
	s.Index = r.next
	r.next++
	r.started = true
	r.emit(s)
	return s.Index
}

// whole emits a part that arrived complete.
func (r *responseMapper) whole(s types.PartStart, p types.AssistantPart, mid ...types.PartDelta) {
	i := r.start(s)
	for _, d := range mid {
		d.Index = i
		r.emit(d)
	}
	r.emit(types.PartEnd{Index: i, Part: p})
}

// run makes sure a run of kind (and, for audio, media type) is open.
func (r *responseMapper) run(kind types.PartKind, mt string) {
	if r.open >= 0 && r.openKind == kind && r.audioType == mt {
		return
	}
	r.closeRun()
	r.openKind, r.audioType = kind, mt
	r.open = r.start(types.PartStart{Kind: kind, MediaType: types.MediaType(mt)})
	if kind == types.KindText {
		r.spans = append(r.spans, &textSpan{index: r.open, offset: r.textLength})
	}
}

// closeRun ends the open run, if any.
func (r *responseMapper) closeRun() {
	if r.open < 0 {
		return
	}
	i, sig := r.open, r.runSig
	r.open, r.runSig = -1, nil
	switch r.openKind {
	case types.KindThinking:
		if len(sig) > 0 {
			r.emit(types.PartDelta{Index: i, Signature: encodeSignature(sig)})
		}
		r.emit(types.PartEnd{Index: i})
	case types.KindAudioOut:
		mt := types.MediaType(r.audioType)
		r.emit(types.PartEnd{Index: i, Part: types.AudioOutPart{Source: types.Bytes(mt, r.audio), AudioMeta: audioMeta(mt)}})
		r.audio = nil
		r.signature(sig)
	default:
		r.emit(types.PartEnd{Index: i})
		r.signature(sig)
	}
	r.audioType = ""
}

// signature emits an empty, signed thinking part for a signature Gemini put
// on a part that has no thinking of its own.
func (r *responseMapper) signature(sig []byte) {
	if len(sig) == 0 {
		return
	}
	s := encodeSignature(sig)
	r.whole(types.PartStart{Kind: types.KindThinking}, types.ThinkingPart{Signature: s}, types.PartDelta{Signature: s})
}

func (r *responseMapper) toolCall(fc *genai.FunctionCall) {
	id := fc.ID
	if id == "" {
		id = types.NewID()
	}
	args := fc.Args
	if args == nil {
		args = map[string]any{}
	}
	raw, _ := json.Marshal(args)
	r.whole(types.PartStart{Kind: types.KindToolCall, ID: id, Name: fc.Name},
		types.ToolCallPart{ID: id, Name: fc.Name, Arguments: args}, types.PartDelta{Args: string(raw)})
}

// media maps generated media: image and video parts whole, audio as a run
// whose chunks are streamed in order.
func (r *responseMapper) media(mime string, data []byte, uri string, sig []byte) error {
	mt := types.MediaType(mime)
	src := types.URL(uri, mt)
	if uri == "" {
		src = types.Bytes(mt, data)
	}
	switch mt.Modality() {
	case types.ModalityAudio:
		if uri == "" {
			r.run(types.KindAudioOut, mime)
			r.audio = append(r.audio, data...)
			r.emit(types.PartDelta{Index: r.open, Data: data})
			if len(sig) > 0 {
				r.runSig = sig
			}
			return nil
		}
		r.closeRun()
		r.whole(types.PartStart{Kind: types.KindAudioOut, MediaType: mt}, types.AudioOutPart{Source: src, AudioMeta: audioMeta(mt)})
		r.signature(sig)
	case types.ModalityImage:
		r.closeRun()
		r.whole(types.PartStart{Kind: types.KindImageOut, MediaType: mt}, types.ImageOutPart{Source: src, Signature: encodeSignature(sig)})
	case types.ModalityVideo:
		r.closeRun()
		r.whole(types.PartStart{Kind: types.KindVideoOut, MediaType: mt}, types.VideoOutPart{Source: src})
		r.signature(sig)
	default:
		return fmt.Errorf("model returned media of type %q, which has no output part", mime)
	}
	return nil
}

// audioMeta reads the sample rate Gemini puts on raw PCM audio
// ("audio/L16;codec=pcm;rate=24000").
func audioMeta(mt types.MediaType) types.AudioMeta {
	var m types.AudioMeta
	_, params, _ := strings.Cut(string(mt), ";")
	for _, kv := range strings.Split(params, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		switch strings.ToLower(k) {
		case "rate":
			m.SampleRate, _ = strconv.Atoi(v)
		case "codec":
			m.Format = v
		}
	}
	if strings.HasPrefix(strings.ToLower(string(mt)), "audio/l16") {
		m.Channels = 1
		if m.Format == "" {
			m.Format = "pcm"
		}
	}
	return m
}

// refusal records a blocked prompt or a response stopped for safety as a
// refusal part. The stream still ends with the content-filter error.
func (r *responseMapper) refusal(text, category string) {
	r.closeRun()
	r.whole(types.PartStart{Kind: types.KindRefusal}, types.RefusalPart{Text: text, Category: category})
}

// finish ends the open run and reports the search the model ran and the
// grounding citations. A citation with a grounding support is anchored to
// the text span it supports; a source no support names is still reported,
// without an anchor.
func (r *responseMapper) finish() {
	r.closeRun()
	md := r.grounding
	if md == nil {
		return
	}
	if call, res, ok := r.server.search(md); ok {
		r.whole(types.PartStart{Kind: types.KindServerToolCall, ID: call.ID, Name: call.Name}, call)
		r.whole(types.PartStart{Kind: types.KindServerToolResult, ID: res.CallID}, res)
	}
	used := map[int32]bool{}
	for _, s := range md.GroundingSupports {
		if s == nil {
			continue
		}
		anchor, start, end := r.anchor(s.Segment)
		for k, ci := range s.GroundingChunkIndices {
			if ci < 0 || int(ci) >= len(md.GroundingChunks) {
				continue
			}
			c, ok := citationOf(md.GroundingChunks[ci])
			if !ok {
				continue
			}
			used[ci] = true
			if anchor != nil {
				c.Start, c.End = start, end
			}
			if k < len(s.ConfidenceScores) {
				if c.Meta == nil {
					c.Meta = map[string]any{}
				}
				c.Meta["confidence"] = s.ConfidenceScores[k]
			}
			r.whole(types.PartStart{Kind: types.KindCitation}, types.CitationPart{Citation: c, Anchor: anchor})
		}
	}
	for i, ch := range md.GroundingChunks {
		if used[int32(i)] { //nolint:gosec // the chunk list is far below 2^31
			continue
		}
		if c, ok := citationOf(ch); ok {
			r.whole(types.PartStart{Kind: types.KindCitation}, types.CitationPart{Citation: c})
		}
	}
}

// anchor locates a grounding segment in the turn's text. Gemini reports
// byte offsets into the response text; when they do not match the
// segment's own text, the text is searched for instead. It returns the
// anchor and the span in the turn's text, or nil when the segment is not
// found.
func (r *responseMapper) anchor(seg *genai.Segment) (*types.Anchor, int, int) {
	if seg == nil || len(r.spans) == 0 {
		return nil, -1, -1
	}
	var all strings.Builder
	for _, s := range r.spans {
		all.WriteString(s.text.String())
	}
	text := all.String()
	start, end := int(seg.StartIndex), int(seg.EndIndex)
	if start < 0 || end <= start || end > len(text) || (seg.Text != "" && text[start:end] != seg.Text) {
		if seg.Text == "" {
			return nil, -1, -1
		}
		i := strings.Index(text, seg.Text)
		if i < 0 {
			return nil, -1, -1
		}
		start, end = i, i+len(seg.Text)
	}
	for _, s := range r.spans {
		n := s.text.Len()
		if start >= s.offset && start < s.offset+n {
			return &types.Anchor{PartIndex: s.index, Start: start - s.offset, End: min(end, s.offset+n) - s.offset}, start, end
		}
	}
	return nil, -1, -1
}

// citationOf converts one grounding chunk: a web page, a retrieved
// document chunk, a place, or an image.
func citationOf(ch *genai.GroundingChunk) (types.Citation, bool) {
	var c types.Citation
	switch {
	case ch == nil:
		return c, false
	case ch.Web != nil && ch.Web.URI != "":
		c = types.NewCitation(types.CitationWeb, ch.Web.URI, ch.Web.Title)
		if ch.Web.Domain != "" {
			c.Meta = map[string]any{"domain": ch.Web.Domain}
		}
	case ch.RetrievedContext != nil && (ch.RetrievedContext.URI != "" || ch.RetrievedContext.Title != ""):
		rc := ch.RetrievedContext
		c = types.NewCitation(types.CitationRetrieval, rc.URI, rc.Title)
		c.Meta = map[string]any{}
		if rc.DocumentName != "" {
			c.Meta["document"] = rc.DocumentName
		}
		if rc.PageNumber != nil {
			c.Meta["page"] = int(*rc.PageNumber)
		}
		if rc.Text != "" {
			c.Quote = rc.Text
		}
	case ch.Maps != nil && (ch.Maps.URI != "" || ch.Maps.Title != ""):
		c = types.NewCitation(types.CitationWeb, ch.Maps.URI, ch.Maps.Title)
		if ch.Maps.PlaceID != "" {
			c.Meta = map[string]any{"place_id": ch.Maps.PlaceID}
		}
	case ch.Image != nil && ch.Image.SourceURI != "":
		c = types.NewCitation(types.CitationWeb, ch.Image.SourceURI, ch.Image.Title)
		if ch.Image.ImageURI != "" {
			c.Meta = map[string]any{"image": ch.Image.ImageURI}
		}
	default:
		return c, false
	}
	c.Producer = providerName
	return c, true
}

// responseParts converts a complete response, as the stream would, and
// returns the finished parts.
func responseParts(cand *genai.Candidate, blocked *genai.GenerateContentResponsePromptFeedback) ([]types.AssistantPart, error) {
	asm := types.NewPartAssembler()
	r := newResponseMapper(asm.Push)
	if err := r.candidate(cand); err != nil {
		return nil, err
	}
	r.finish()
	switch {
	case blocked != nil:
		r.refusal(blocked.BlockReasonMessage, string(blocked.BlockReason))
	case cand != nil && types.IsContentFilterFinishReason(string(cand.FinishReason)):
		r.refusal(cand.FinishMessage, string(cand.FinishReason))
	}
	return asm.Parts(), nil
}
