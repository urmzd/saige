package extractor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/types"
)

// One extractor registry serves both sides of the SDK. Auto turns raw bytes
// into a RAG document for ingestion, and the same registry, through
// PartExtractor, is an agent/types.Extractor that turns a media part into
// text parts, which agent/convert's extract converters and
// agent.WithExtractors run. A part extractor registered with RegisterParts,
// such as an OCR or a vision describer, likewise ingests documents.

// ErrNoText reports a document from which no text could be extracted.
var ErrNoText = errors.New("no text could be extracted")

// PartExtractor returns the registry as an agent/types.Extractor: the bytes
// are extracted by the registered extractor for their media type, and the
// document's title and the text of each section, under its heading (one per
// page for a PDF), become one text part. A document with no text is an
// error wrapping [ErrNoText], so a scanned PDF falls through to the next
// permitted conversion instead of reaching a model as an empty part.
func (a *Auto) PartExtractor() agenttypes.Extractor {
	return agenttypes.ExtractorFunc(func(ctx context.Context, data []byte, mt agenttypes.MediaType) ([]agenttypes.UserPart, error) {
		doc, err := a.Extract(ctx, &types.RawDocument{MIMEType: normalizeMIME(string(mt)), Data: data})
		if err != nil {
			return nil, err
		}
		text := DocumentText(doc)
		if strings.TrimSpace(text) == "" {
			return nil, ErrNoText
		}
		return []agenttypes.UserPart{agenttypes.Text(text)}, nil
	})
}

// MediaTypes lists the media types with a registered extractor, sorted.
// Text types other than these are read by the plain-text fallback.
func (a *Auto) MediaTypes() []agenttypes.MediaType {
	out := make([]agenttypes.MediaType, 0, len(a.extractors))
	for mt := range a.extractors {
		out = append(out, agenttypes.MediaType(mt))
	}
	slices.Sort(out)
	return out
}

// Extractors returns the registry as one agent/types.Extractor per
// registered media type, for agent.WithExtractors.
func (a *Auto) Extractors() map[agenttypes.MediaType]agenttypes.Extractor {
	ex := a.PartExtractor()
	out := make(map[agenttypes.MediaType]agenttypes.Extractor, len(a.extractors))
	for _, mt := range a.MediaTypes() {
		out[mt] = ex
	}
	return out
}

// RegisterParts registers a part extractor (an agent/types.Extractor, such
// as an OCR or a converter-backed describer) for a media type. See
// [FromParts] for the document it builds.
func (a *Auto) RegisterParts(mimeType string, ex agenttypes.Extractor) {
	a.Register(normalizeMIME(mimeType), FromParts(ex))
}

// RegisterImages registers [Image] for the common image types (PNG, JPEG,
// GIF and WebP), with describe producing the text that is embedded and
// searched. Without a describer an image cannot be ingested: there is no
// text to embed.
func (a *Auto) RegisterImages(describe agenttypes.Extractor) {
	img := &Image{Describe: describe}
	for _, mt := range ImageMediaTypes {
		a.Register(mt, img)
	}
}

// ImageMediaTypes are the image types RegisterImages registers.
var ImageMediaTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// DocumentText renders a document as text: its title as a heading, then
// each section's text under its heading.
func DocumentText(doc *types.Document) string {
	var b strings.Builder
	if doc.Title != "" {
		fmt.Fprintf(&b, "# %s\n", doc.Title)
	}
	for _, s := range doc.Sections {
		var body []string
		for _, v := range s.Variants {
			if v.Text != "" {
				body = append(body, v.Text)
			}
		}
		if len(body) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if s.Heading != "" {
			fmt.Fprintf(&b, "## %s\n", s.Heading)
		}
		b.WriteString(strings.Join(body, "\n"))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// FromParts adapts a part extractor to a ContentExtractor. The document has
// one section holding a variant per part the extractor returns (see
// types.VariantFromPart), with the raw document's media type and metadata.
// An extraction that yields no part is an error wrapping [ErrNoText].
func FromParts(ex agenttypes.Extractor) types.ContentExtractor {
	return partsExtractor{ex: ex}
}

type partsExtractor struct{ ex agenttypes.Extractor }

func (p partsExtractor) Extract(ctx context.Context, raw *types.RawDocument) (*types.Document, error) {
	parts, err := p.ex.Extract(ctx, raw.Data, agenttypes.MediaType(raw.MIMEType))
	if err != nil {
		return nil, err
	}
	variants := make([]types.ContentVariant, 0, len(parts))
	for _, part := range parts {
		v, err := types.VariantFromPart(part)
		if err != nil {
			return nil, err
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		return nil, ErrNoText
	}
	return singleSection(raw, "", variants), nil
}

// Image ingests an image as one section holding one image variant: the
// image's bytes, with the text Describe produces for it as the variant's
// text. The text is what gets embedded and searched (the embedder registry
// routes an image variant without an image embedder to its fallback, which
// embeds the text), and a hit returns the image itself to an agent.
type Image struct {
	// Describe turns the image into text, such as a vision model through
	// agent/convert.Describe or an OCR. Required.
	Describe agenttypes.Extractor
}

// Extract implements types.ContentExtractor.
func (e *Image) Extract(ctx context.Context, raw *types.RawDocument) (*types.Document, error) {
	if e.Describe == nil {
		return nil, fmt.Errorf("image %s: no describer configured: %w", raw.SourceURI, types.ErrUnsupportedMIMEType)
	}
	parts, err := e.Describe.Extract(ctx, raw.Data, agenttypes.MediaType(raw.MIMEType))
	if err != nil {
		return nil, fmt.Errorf("describe image: %w", err)
	}
	var texts []string
	for _, p := range parts {
		if t, ok := p.(agenttypes.TextPart); ok && strings.TrimSpace(t.Text) != "" {
			texts = append(texts, strings.TrimSpace(t.Text))
		}
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("describe image: %w", ErrNoText)
	}
	text := strings.Join(texts, "\n")
	img := types.ContentVariant{ContentType: types.ContentImage, MIMEType: normalizeMIME(raw.MIMEType), Data: raw.Data, Text: text}
	return singleSection(raw, titleFromText(text), []types.ContentVariant{img}), nil
}

// singleSection builds a document with one section holding variants,
// assigning UUIDs and copying the raw document's metadata to each variant.
func singleSection(raw *types.RawDocument, title string, variants []types.ContentVariant) *types.Document {
	now := time.Now()
	docUUID, secUUID := uuid.New().String(), uuid.New().String()
	for i := range variants {
		variants[i].UUID = uuid.New().String()
		variants[i].SectionUUID = secUUID
		if variants[i].MIMEType == "" {
			variants[i].MIMEType = normalizeMIME(raw.MIMEType)
		}
		variants[i].Metadata = raw.Metadata
	}
	return &types.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     title,
		Metadata:  raw.Metadata,
		Sections:  []types.Section{{UUID: secUUID, DocumentUUID: docUUID, Variants: variants}},
		CreatedAt: now,
		UpdatedAt: now,
	}
}
