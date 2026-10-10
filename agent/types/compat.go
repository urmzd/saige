package types

import "encoding/json"

// Compatibility shims for the content API that typed parts replaced. They
// build parts and are removed in the next minor release.

// NewSystemMessage creates a SystemMessage with a single text part.
//
// Deprecated: Use SystemMsg(Text(text)).
func NewSystemMessage(text string) SystemMessage { return SystemMsg(Text(text)) }

// NewUserMessage creates a UserMessage with a single text part.
//
// Deprecated: Use UserMsg(Text(text)).
func NewUserMessage(text string) UserMessage { return UserMsg(Text(text)) }

// NewAssistantMessage creates an AssistantMessage with a single text part.
//
// Deprecated: Use AssistantMsg(Text(text)).
func NewAssistantMessage(text string) AssistantMessage { return AssistantMsg(Text(text)) }

// NewToolResultMessage creates a SystemMessage holding tool results.
//
// Deprecated: Use ToolResults.
func NewToolResultMessage(results ...ToolResultPart) SystemMessage { return ToolResults(results...) }

// NewUserToolResultMessage creates a UserMessage holding tool results.
//
// Deprecated: Use UserToolResults.
func NewUserToolResultMessage(results ...ToolResultPart) UserMessage {
	return UserToolResults(results...)
}

// NewFileMessage creates a UserMessage with one media part reachable at uri.
//
// Deprecated: Use UserMsg(Media(URL(uri, mediaType))).
func NewFileMessage(uri string, mediaType ...MediaType) UserMessage {
	return UserMsg(Media(URL(uri, mediaType...)))
}

// NewUserMessageWithFiles creates a UserMessage with text followed by media
// parts.
//
// Deprecated: Use UserMsg with Text and Media parts.
func NewUserMessageWithFiles(text string, files ...FileContent) UserMessage {
	parts := make([]UserPart, 0, 1+len(files))
	if text != "" {
		parts = append(parts, Text(text))
	}
	for _, f := range files {
		parts = append(parts, f.Part())
	}
	return UserMsg(parts...)
}

// Deprecated aliases for the metadata parts.
type (
	// Deprecated: Use ConfigPart.
	ConfigContent = ConfigPart
	// Deprecated: Use RoutePart.
	RouteContent = RoutePart
	// Deprecated: Use SteerPart.
	SteerContent = SteerPart
	// Deprecated: Use TruncationPart.
	TruncationContent = TruncationPart
	// Deprecated: Use HandoffPart.
	HandoffContent = HandoffPart
	// Deprecated: Use FeedbackPart.
	FeedbackContent = FeedbackPart
	// Deprecated: Use ApprovalPart.
	ApprovalContent = ApprovalPart
	// Deprecated: Use GuardrailPart.
	GuardrailContent = GuardrailPart
	// Deprecated: Use CompactionPart.
	CompactionContent = CompactionPart
)

// RouteContentFrom records the configuration a RouteDelta names.
//
// Deprecated: Use RoutePartFrom.
func RouteContentFrom(r RouteDelta) RoutePart { return RoutePartFrom(r) }

// IsMetadataContent reports whether c is a metadata part.
//
// Deprecated: Use IsMetadata.
func IsMetadataContent(c any) bool {
	p, ok := c.(Part)
	return ok && IsMetadata(p)
}

// FileContent is the attachment form the content API used. It is no longer
// a message part; Part converts it.
//
// Deprecated: Use a media part (Image, Audio, Video, Document, File or
// Media) with a Source.
type FileContent struct {
	URI       string    `json:"uri"`
	MediaType MediaType `json:"media_type,omitempty"`
	Data      []byte    `json:"-"`
	Filename  string    `json:"filename,omitempty"`
}

// Source returns the media source the attachment names.
func (f FileContent) Source() Source {
	s := Source{URI: f.URI, MediaType: f.MediaType, Filename: f.Filename}
	if len(f.Data) > 0 {
		s = Bytes(f.MediaType, f.Data).With(s)
	}
	return s
}

// Part returns the media part for the attachment, by its media type.
func (f FileContent) Part() UserPart { return Media(f.Source()) }

// ToolResultBlockKind enumerates the kinds of a ToolResultBlock.
//
// Deprecated: Tool output is a list of ToolOutputPart.
type ToolResultBlockKind string

// Deprecated: Tool output is a list of ToolOutputPart.
const (
	ToolResultBlockText  ToolResultBlockKind = "text"
	ToolResultBlockImage ToolResultBlockKind = "image"
	ToolResultBlockFile  ToolResultBlockKind = "file"
	ToolResultBlockJSON  ToolResultBlockKind = "json"
)

// ToolResultBlock is one block of the rich tool output the content API
// used. Part converts it.
//
// Deprecated: Tool output is a list of ToolOutputPart.
type ToolResultBlock struct {
	Kind      ToolResultBlockKind `json:"kind"`
	Text      string              `json:"text,omitempty"`
	MediaType MediaType           `json:"media_type,omitempty"`
	URI       string              `json:"uri,omitempty"`
	Filename  string              `json:"filename,omitempty"`
	Data      []byte              `json:"-"`
	JSON      json.RawMessage     `json:"json,omitempty"`
}

// Part returns the tool output part for the block. A file block becomes a
// document, audio or opaque file part by its media type.
func (b ToolResultBlock) Part() ToolOutputPart {
	switch b.Kind {
	case ToolResultBlockText:
		return Text(b.Text)
	case ToolResultBlockJSON:
		return JSONPart{JSON: b.JSON}
	}
	src := FileContent{URI: b.URI, MediaType: b.MediaType, Filename: b.Filename, Data: b.Data}.Source()
	if b.Kind == ToolResultBlockImage {
		return Image(src)
	}
	switch src.MediaType.Modality() {
	case ModalityImage:
		return Image(src)
	case ModalityAudio:
		return Audio(src)
	case ModalityDocument:
		return Document(src)
	default:
		return File(src)
	}
}
