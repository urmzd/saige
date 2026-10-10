package types

import (
	"encoding/json"
	"strings"
	"time"
)

// PartKind discriminates a Part. Kinds are part of the persisted and wire
// contract: never rename one.
type PartKind string

// Input part kinds.
const (
	KindText       PartKind = "text"
	KindImage      PartKind = "image"
	KindAudio      PartKind = "audio"
	KindVideo      PartKind = "video"
	KindDocument   PartKind = "document"
	KindFile       PartKind = "file"
	KindToolResult PartKind = "tool_result"
	// KindJSON is structured tool output. It appears only inside a
	// ToolResultPart.
	KindJSON PartKind = "json"
)

// Assistant part kinds.
const (
	KindThinking         PartKind = "thinking"
	KindToolCall         PartKind = "tool_call"
	KindServerToolCall   PartKind = "server_tool_call"
	KindServerToolResult PartKind = "server_tool_result"
	KindCitation         PartKind = "citation"
	KindAudioOut         PartKind = "audio_out"
	KindImageOut         PartKind = "image_out"
	KindVideoOut         PartKind = "video_out"
	KindRefusal          PartKind = "refusal"
)

// Metadata part kinds. The agent loop resolves or records these itself and
// never sends them to a provider.
const (
	KindConfig     PartKind = "config"
	KindRoute      PartKind = "route"
	KindSteer      PartKind = "steer"
	KindTruncation PartKind = "truncation"
	KindApproval   PartKind = "approval"
	KindHandoff    PartKind = "handoff"
	KindFeedback   PartKind = "feedback"
	KindCompaction PartKind = "compaction"
	KindGuardrail  PartKind = "guardrail"
)

// Part is one ordered element of a message. It is sealed: only this package
// implements it, so a type switch over the kinds below is exhaustive.
type Part interface {
	Kind() PartKind
	isPart()
}

// SystemPart is a part allowed in a SystemMessage.
type SystemPart interface {
	Part
	isSystemPart()
}

// UserPart is a part allowed in a UserMessage.
type UserPart interface {
	Part
	isUserPart()
}

// AssistantPart is a part allowed in an AssistantMessage.
type AssistantPart interface {
	Part
	isAssistantPart()
}

// ToolOutputPart is a part a ToolResultPart may contain: text, JSON, an
// image, audio, a document, or an opaque file.
type ToolOutputPart interface {
	Part
	isToolOutputPart()
}

// IsMetadata reports whether p is run metadata that the agent loop resolves
// or records itself and never sends to a provider.
func IsMetadata(p Part) bool {
	switch p.(type) {
	case ConfigPart, RoutePart, SteerPart, TruncationPart, ApprovalPart, HandoffPart, FeedbackPart,
		CompactionPart, GuardrailPart:
		return true
	default:
		return false
	}
}

// IsMedia reports whether p carries media: an image, audio, video, document
// or file input, or media the model produced.
func IsMedia(p Part) bool {
	_, ok := SourceOf(p)
	return ok
}

// SourceOf returns the media source of a media part. It reports false for
// every other kind.
func SourceOf(p Part) (Source, bool) {
	switch v := p.(type) {
	case ImagePart:
		return v.Source, true
	case AudioPart:
		return v.Source, true
	case VideoPart:
		return v.Source, true
	case DocumentPart:
		return v.Source, true
	case FilePart:
		return v.Source, true
	case AudioOutPart:
		return v.Source, true
	case ImageOutPart:
		return v.Source, true
	case VideoOutPart:
		return v.Source, true
	default:
		return Source{}, false
	}
}

// ── Text and structured output ───────────────────────────────────────

// TextPart holds plain text. It is valid in every role and in tool output.
type TextPart struct {
	Text string `json:"text"`
}

func (TextPart) Kind() PartKind    { return KindText }
func (TextPart) isPart()           {}
func (TextPart) isSystemPart()     {}
func (TextPart) isUserPart()       {}
func (TextPart) isAssistantPart()  {}
func (TextPart) isToolOutputPart() {}

// JSONPart is structured tool output. Adapters send it to the model as its
// JSON text.
type JSONPart struct {
	JSON json.RawMessage `json:"json"`
}

func (JSONPart) Kind() PartKind    { return KindJSON }
func (JSONPart) isPart()           {}
func (JSONPart) isToolOutputPart() {}

// ── Media input ──────────────────────────────────────────────────────

// ImageMeta describes an image. Detail is a resolution hint for the model:
// "auto", "low" or "high".
type ImageMeta struct {
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// AudioMeta describes an audio clip. Format names the encoding, such as
// "wav" or "mp3", when the media type alone does not.
type AudioMeta struct {
	Duration   time.Duration `json:"duration,omitempty"`
	SampleRate int           `json:"sample_rate,omitempty"`
	Channels   int           `json:"channels,omitempty"`
	Format     string        `json:"format,omitempty"`
}

// VideoMeta describes a video. ClipStart and ClipEnd select a span to send;
// FPS is the sampling rate a provider should use.
type VideoMeta struct {
	Duration  time.Duration `json:"duration,omitempty"`
	Frames    int           `json:"frames,omitempty"`
	FPS       float64       `json:"fps,omitempty"`
	Width     int           `json:"width,omitempty"`
	Height    int           `json:"height,omitempty"`
	ClipStart time.Duration `json:"clip_start,omitempty"`
	ClipEnd   time.Duration `json:"clip_end,omitempty"`
}

// DocumentMeta describes a document. Title and Context are passed to
// providers that accept them; Citations asks a provider that can cite
// document passages to do so.
type DocumentMeta struct {
	Pages     int    `json:"pages,omitempty"`
	Title     string `json:"title,omitempty"`
	Context   string `json:"context,omitempty"`
	Citations bool   `json:"citations,omitempty"`
}

// ImagePart is an image input.
type ImagePart struct {
	Source    Source `json:"source"`
	ImageMeta `json:"image,omitzero"`
}

func (ImagePart) Kind() PartKind    { return KindImage }
func (ImagePart) isPart()           {}
func (ImagePart) isUserPart()       {}
func (ImagePart) isToolOutputPart() {}

// AudioPart is an audio input.
type AudioPart struct {
	Source    Source `json:"source"`
	AudioMeta `json:"audio,omitzero"`
}

func (AudioPart) Kind() PartKind    { return KindAudio }
func (AudioPart) isPart()           {}
func (AudioPart) isUserPart()       {}
func (AudioPart) isToolOutputPart() {}

// VideoPart is a video input.
type VideoPart struct {
	Source    Source `json:"source"`
	VideoMeta `json:"video,omitzero"`
}

func (VideoPart) Kind() PartKind { return KindVideo }
func (VideoPart) isPart()        {}
func (VideoPart) isUserPart()    {}

// DocumentPart is a document input: a PDF, plain text, or another format a
// provider reads as a document.
type DocumentPart struct {
	Source       Source `json:"source"`
	DocumentMeta `json:"document,omitzero"`
}

func (DocumentPart) Kind() PartKind    { return KindDocument }
func (DocumentPart) isPart()           {}
func (DocumentPart) isUserPart()       {}
func (DocumentPart) isToolOutputPart() {}

// FilePart is an opaque file, such as an input for provider-side code
// execution. No provider reads it as content.
type FilePart struct {
	Source Source `json:"source"`
}

func (FilePart) Kind() PartKind    { return KindFile }
func (FilePart) isPart()           {}
func (FilePart) isUserPart()       {}
func (FilePart) isToolOutputPart() {}

// ── Tool results ─────────────────────────────────────────────────────

// ToolResultPart carries the result of a tool call. It is valid in a
// SystemMessage (automatic execution) or a UserMessage (a human answered the
// call).
type ToolResultPart struct {
	CallID string `json:"call_id"`
	// Parts is the output in order: text, JSON, images, documents, audio or
	// files.
	Parts   []ToolOutputPart `json:"parts"`
	IsError bool             `json:"is_error,omitempty"`
	// Citations attributes this result to its sources, carrying the ordinals
	// the run's CitationRegistry assigned. Persisted, so a restored
	// conversation keeps the numbering its earlier answers referenced.
	Citations []Citation `json:"citations,omitempty"`
	// ToolVersion is the version the tool reported when it ran (see
	// ToolVersion), so a transcript names the schema and prompt that
	// produced each result. Empty for a tool that reports none.
	ToolVersion string `json:"tool_version,omitempty"`
}

func (ToolResultPart) Kind() PartKind { return KindToolResult }
func (ToolResultPart) isPart()        {}
func (ToolResultPart) isSystemPart()  {}
func (ToolResultPart) isUserPart()    {}

// Text is the text projection of the result: its text and JSON parts joined
// by newlines. Media parts are not included.
func (r ToolResultPart) Text() string { return outputText(r.Parts) }

// HasMedia reports whether the result carries a media part.
func (r ToolResultPart) HasMedia() bool { return outputHasMedia(r.Parts) }

func outputText(parts []ToolOutputPart) string {
	var texts []string
	for _, p := range parts {
		switch v := p.(type) {
		case TextPart:
			texts = append(texts, v.Text)
		case JSONPart:
			texts = append(texts, string(v.JSON))
		}
	}
	return strings.Join(texts, "\n")
}

func outputHasMedia(parts []ToolOutputPart) bool {
	for _, p := range parts {
		if IsMedia(p) {
			return true
		}
	}
	return false
}

// ── Assistant output ─────────────────────────────────────────────────

// ThinkingPart is a reasoning block. Signature is the opaque token a
// provider needs to accept the block back in a later turn (an Anthropic
// signature, a Gemini thought signature, or OpenAI encrypted content).
type ThinkingPart struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
	// Redacted marks a block the provider encrypted; Text is empty and
	// Signature carries the data.
	Redacted bool `json:"redacted,omitempty"`
	// Summary marks a summary of the reasoning rather than the reasoning.
	Summary bool `json:"summary,omitempty"`
}

func (ThinkingPart) Kind() PartKind   { return KindThinking }
func (ThinkingPart) isPart()          {}
func (ThinkingPart) isAssistantPart() {}

// ToolCallPart is the model's request to call a tool.
type ToolCallPart struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// ArgumentsError is set when the model's argument text could not be
	// decoded. Arguments is then nil, and the agent loop answers the call with
	// an error result instead of running the tool.
	ArgumentsError string `json:"arguments_error,omitempty"`
}

func (ToolCallPart) Kind() PartKind   { return KindToolCall }
func (ToolCallPart) isPart()          {}
func (ToolCallPart) isAssistantPart() {}

// ServerToolCallPart records a tool call the provider executed itself, such
// as a web search or code execution. Nothing runs locally.
type ServerToolCallPart struct {
	ID       string         `json:"id"`
	ToolKind ServerToolKind `json:"tool_kind"`
	Name     string         `json:"name,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
}

func (ServerToolCallPart) Kind() PartKind   { return KindServerToolCall }
func (ServerToolCallPart) isPart()          {}
func (ServerToolCallPart) isAssistantPart() {}

// ServerToolResultPart is the outcome of a ServerToolCallPart. It is a part
// of its own, next to the call; PairServerTools joins them for display.
type ServerToolResultPart struct {
	CallID   string          `json:"call_id"`
	ToolKind ServerToolKind  `json:"tool_kind"`
	Text     string          `json:"text,omitempty"`   // human-readable projection
	Result   json.RawMessage `json:"result,omitempty"` // provider-native payload
	IsError  bool            `json:"is_error,omitempty"`
	// Outputs are media parts the tool produced, such as files written by
	// code execution.
	Outputs []Part `json:"outputs,omitempty"`
}

func (ServerToolResultPart) Kind() PartKind   { return KindServerToolResult }
func (ServerToolResultPart) isPart()          {}
func (ServerToolResultPart) isAssistantPart() {}

// ServerToolPair is a server tool call with its result, when one arrived.
type ServerToolPair struct {
	Call   ServerToolCallPart
	Result *ServerToolResultPart
}

// PairServerTools joins each server tool call in parts with its result. A
// result whose call is absent is paired with a call that has only its ID and
// kind. The order follows the calls.
func PairServerTools[P Part](parts []P) []ServerToolPair {
	var pairs []ServerToolPair
	at := map[string]int{}
	for _, p := range parts {
		switch v := any(p).(type) {
		case ServerToolCallPart:
			at[v.ID] = len(pairs)
			pairs = append(pairs, ServerToolPair{Call: v})
		case ServerToolResultPart:
			r := v
			if i, ok := at[v.CallID]; ok && pairs[i].Result == nil {
				pairs[i].Result = &r
				continue
			}
			pairs = append(pairs, ServerToolPair{Call: ServerToolCallPart{ID: v.CallID, ToolKind: v.ToolKind}, Result: &r})
		}
	}
	return pairs
}

// Anchor ties a citation to a span of a text part in the same message:
// PartIndex is the part's position, Start and End are byte offsets into its
// text.
type Anchor struct {
	PartIndex int `json:"part_index"`
	Start     int `json:"start"`
	End       int `json:"end"`
}

// CitationPart is one attribution the model produced. It is stored with the
// message, so a restored conversation still knows its sources.
type CitationPart struct {
	Citation Citation `json:"citation"`
	Anchor   *Anchor  `json:"anchor,omitempty"`
}

func (CitationPart) Kind() PartKind   { return KindCitation }
func (CitationPart) isPart()          {}
func (CitationPart) isAssistantPart() {}

// AudioOutPart is audio the model produced. VendorID and ExpiresAt name a
// provider-side copy that a later turn may reference instead of the bytes.
type AudioOutPart struct {
	Source     Source `json:"source"`
	AudioMeta  `json:"audio,omitzero"`
	Transcript string    `json:"transcript,omitempty"`
	VendorID   string    `json:"vendor_id,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
}

func (AudioOutPart) Kind() PartKind   { return KindAudioOut }
func (AudioOutPart) isPart()          {}
func (AudioOutPart) isAssistantPart() {}

// ImageOutPart is an image the model produced.
type ImageOutPart struct {
	Source        Source `json:"source"`
	ImageMeta     `json:"image,omitzero"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
	Signature     string `json:"signature,omitempty"`
}

func (ImageOutPart) Kind() PartKind   { return KindImageOut }
func (ImageOutPart) isPart()          {}
func (ImageOutPart) isAssistantPart() {}

// VideoOutPart is a video the model produced. It is reserved: no adapter
// produces it yet. Operation names a long-running generation job.
type VideoOutPart struct {
	Source    Source `json:"source"`
	VideoMeta `json:"video,omitzero"`
	Operation string `json:"operation,omitempty"`
}

func (VideoOutPart) Kind() PartKind   { return KindVideoOut }
func (VideoOutPart) isPart()          {}
func (VideoOutPart) isAssistantPart() {}

// RefusalPart records that the model declined to answer. The stream also
// ends with an error that matches ErrContentFiltered, so routing treats the
// refusal as it always has.
type RefusalPart struct {
	Text     string `json:"text,omitempty"`
	Category string `json:"category,omitempty"`
}

func (RefusalPart) Kind() PartKind   { return KindRefusal }
func (RefusalPart) isPart()          {}
func (RefusalPart) isAssistantPart() {}

// ── Constructors ─────────────────────────────────────────────────────

// Text returns a text part.
func Text(s string) TextPart { return TextPart{Text: s} }

// Image returns an image part. At most one meta is used.
func Image(src Source, meta ...ImageMeta) ImagePart {
	p := ImagePart{Source: src}
	if len(meta) > 0 {
		p.ImageMeta = meta[0]
	}
	return p
}

// Audio returns an audio part. At most one meta is used.
func Audio(src Source, meta ...AudioMeta) AudioPart {
	p := AudioPart{Source: src}
	if len(meta) > 0 {
		p.AudioMeta = meta[0]
	}
	return p
}

// Video returns a video part. At most one meta is used.
func Video(src Source, meta ...VideoMeta) VideoPart {
	p := VideoPart{Source: src}
	if len(meta) > 0 {
		p.VideoMeta = meta[0]
	}
	return p
}

// Document returns a document part. At most one meta is used.
func Document(src Source, meta ...DocumentMeta) DocumentPart {
	p := DocumentPart{Source: src}
	if len(meta) > 0 {
		p.DocumentMeta = meta[0]
	}
	return p
}

// File returns an opaque file part.
func File(src Source) FilePart { return FilePart{Source: src} }

// Media returns the part for src by its media type: image/* is an image,
// audio/* audio, video/* video, PDF and text/* a document, and anything else
// an opaque file.
func Media(src Source) UserPart {
	switch src.MediaType.Modality() {
	case ModalityImage:
		return Image(src)
	case ModalityAudio:
		return Audio(src)
	case ModalityVideo:
		return Video(src)
	case ModalityDocument:
		return Document(src)
	default:
		return File(src)
	}
}

// ToolOK returns a successful tool result.
func ToolOK(callID string, parts ...ToolOutputPart) ToolResultPart {
	return ToolResultPart{CallID: callID, Parts: parts}
}

// ToolErr returns a failed tool result whose text is msg.
func ToolErr(callID, msg string) ToolResultPart {
	return ToolResultPart{CallID: callID, Parts: []ToolOutputPart{Text(msg)}, IsError: true}
}

// JSON encodes v as a JSON tool output part.
func JSON(v any) (JSONPart, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return JSONPart{}, err
	}
	return JSONPart{JSON: b}, nil
}
