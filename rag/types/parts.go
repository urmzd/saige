package types

import (
	"errors"
	"fmt"

	agenttypes "github.com/urmzd/saige/agent/types"
)

// ErrVariantPart reports a part that cannot become a content variant, or a
// variant that cannot become a part.
var ErrVariantPart = errors.New("no content variant for this part")

// contentTypeOf maps a part kind to the variant content type that holds it.
func contentTypeOf(k agenttypes.PartKind) (ContentType, bool) {
	switch k {
	case agenttypes.KindText:
		return ContentText, true
	case agenttypes.KindImage:
		return ContentImage, true
	case agenttypes.KindAudio:
		return ContentAudio, true
	case agenttypes.KindVideo:
		return ContentVideo, true
	case agenttypes.KindDocument:
		return ContentDocument, true
	}
	return "", false
}

// Part returns the variant as a message part: a text part for text and
// table variants, and a media part holding the variant's bytes for image,
// audio, video and document variants. A media variant without bytes
// returns a text part with its text when it has some, such as an image
// stored only by its description, and is otherwise an error wrapping
// [ErrVariantPart].
func (v ContentVariant) Part() (agenttypes.UserPart, error) {
	switch v.ContentType {
	case ContentText, ContentTable, "":
		return agenttypes.Text(v.Text), nil
	case ContentImage, ContentAudio, ContentVideo, ContentDocument:
	default:
		return nil, fmt.Errorf("%w: content type %q", ErrVariantPart, v.ContentType)
	}
	if len(v.Data) == 0 {
		if v.Text != "" {
			return agenttypes.Text(v.Text), nil
		}
		return nil, fmt.Errorf("%w: %s variant %s has no data", ErrVariantPart, v.ContentType, v.UUID)
	}
	src := agenttypes.Bytes(agenttypes.MediaType(v.MIMEType), v.Data)
	switch v.ContentType {
	case ContentImage:
		return agenttypes.Image(src), nil
	case ContentAudio:
		return agenttypes.Audio(src), nil
	case ContentVideo:
		return agenttypes.Video(src), nil
	default:
		return agenttypes.Document(src), nil
	}
}

// VariantFromPart returns the content variant that holds p: a text variant
// for a text part, and a media variant with the part's bytes for an image,
// audio, video or document part. The variant has no UUIDs; the caller
// assigns them. A media part without inline bytes, and any other kind of
// part, is an error wrapping [ErrVariantPart]: a variant stores the media
// itself, not a reference to it.
func VariantFromPart(p agenttypes.UserPart) (ContentVariant, error) {
	if p == nil {
		return ContentVariant{}, fmt.Errorf("%w: nil part", ErrVariantPart)
	}
	ct, ok := contentTypeOf(p.Kind())
	if !ok {
		return ContentVariant{}, fmt.Errorf("%w: %s part", ErrVariantPart, p.Kind())
	}
	if t, isText := p.(agenttypes.TextPart); isText {
		return ContentVariant{ContentType: ContentText, MIMEType: "text/plain", Text: t.Text}, nil
	}
	src, _ := agenttypes.SourceOf(p)
	if len(src.Inline) == 0 {
		return ContentVariant{}, fmt.Errorf("%w: %s part has no inline bytes", ErrVariantPart, p.Kind())
	}
	return ContentVariant{ContentType: ct, MIMEType: string(src.MediaType), Data: src.Inline}, nil
}
