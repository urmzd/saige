package types

import "strings"

// MediaType is a MIME type.
type MediaType string

// Media types the adapters map.
const (
	MediaJPEG MediaType = "image/jpeg"
	MediaPNG  MediaType = "image/png"
	MediaGIF  MediaType = "image/gif"
	MediaWebP MediaType = "image/webp"
	MediaPDF  MediaType = "application/pdf"
	MediaCSV  MediaType = "text/csv"
	MediaMP3  MediaType = "audio/mpeg"
	MediaWAV  MediaType = "audio/wav"
	MediaMP4  MediaType = "video/mp4"
	MediaDOCX MediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MediaXLSX MediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MediaPPTX MediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MediaHTML MediaType = "text/html"
	MediaText MediaType = "text/plain"
	MediaJSON MediaType = "application/json"
)

// Modality is a class of content a model can take or produce.
type Modality string

// Modalities.
const (
	ModalityText     Modality = "text"
	ModalityImage    Modality = "image"
	ModalityAudio    Modality = "audio"
	ModalityVideo    Modality = "video"
	ModalityDocument Modality = "document"
	ModalityFile     Modality = "file"
)

// Modality classifies a media type: image/*, audio/* and video/* by their
// top-level type, PDF and text/* as documents, and anything else as an
// opaque file. Parameters such as "; charset=utf-8" are ignored.
func (mt MediaType) Modality() Modality {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(string(mt))), ";")
	base = strings.TrimSpace(base)
	switch {
	case strings.HasPrefix(base, "image/"):
		return ModalityImage
	case strings.HasPrefix(base, "audio/"):
		return ModalityAudio
	case strings.HasPrefix(base, "video/"):
		return ModalityVideo
	case base == string(MediaPDF), strings.HasPrefix(base, "text/"):
		return ModalityDocument
	default:
		return ModalityFile
	}
}

// PartModality returns the modality of a content part: text for text,
// JSON, thinking, refusal and tool parts, the media modality for media
// parts. It reports false for metadata parts and citations.
func PartModality(p Part) (Modality, bool) {
	switch p.(type) {
	case TextPart, JSONPart, ThinkingPart, RefusalPart, ToolCallPart, ServerToolCallPart, ServerToolResultPart:
		return ModalityText, true
	case ImagePart, ImageOutPart:
		return ModalityImage, true
	case AudioPart, AudioOutPart:
		return ModalityAudio, true
	case VideoPart, VideoOutPart:
		return ModalityVideo, true
	case DocumentPart:
		return ModalityDocument, true
	case FilePart:
		return ModalityFile, true
	default:
		return "", false
	}
}
