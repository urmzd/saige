package convert

import (
	"context"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/extractor"
)

// extract runs a types.Extractor: a document (or any media type it is
// registered for) becomes the parts the extractor returns, usually text.
// It calls no model and costs nothing.
type extract struct {
	name, version string
	ex            types.Extractor
	media         []types.MediaType
}

// Extract returns an extract converter that runs ex on parts of the given
// media types, or on every document part when none are given. Its name is
// "extract" and its version "1"; use ExtractWith to name a converter whose
// output differs from another's for the same bytes.
func Extract(ex types.Extractor, media ...types.MediaType) types.Converter {
	return ExtractWith("extract", "1", ex, media...)
}

// ExtractWith is Extract with an explicit name and version, which key the
// conversion cache and the durable step.
func ExtractWith(name, version string, ex types.Extractor, media ...types.MediaType) types.Converter {
	return &extract{name: name, version: version, ex: ex, media: slices.Clone(media)}
}

func (e *extract) Name() string                         { return e.name }
func (e *extract) Version() string                      { return e.version }
func (e *extract) Action() types.ModalityAction         { return types.ActExtract }
func (e *extract) Produces(types.Part) []types.Modality { return []types.Modality{types.ModalityText} }

func (e *extract) Accepts(p types.Part) bool {
	src, ok := types.SourceOf(p)
	if !ok || len(src.Inline) == 0 {
		return false
	}
	if len(e.media) == 0 {
		_, doc := p.(types.DocumentPart)
		return doc
	}
	return slices.Contains(e.media, baseType(src.MediaType))
}

func (e *extract) Estimate(types.Part, types.Offering) (types.ConversionEstimate, error) {
	return types.ConversionEstimate{}, nil
}

func (e *extract) Convert(ctx context.Context, p types.Part, _ types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	src, _ := types.SourceOf(p)
	parts, err := e.ex.Extract(ctx, src.Inline, src.MediaType)
	if err != nil {
		return nil, types.ConversionUsage{}, err
	}
	out := make([]types.Part, len(parts))
	for i, up := range parts {
		out[i] = up
	}
	return out, types.ConversionUsage{}, nil
}

// baseType drops media type parameters such as "; charset=utf-8".
func baseType(mt types.MediaType) types.MediaType {
	base, _, _ := strings.Cut(string(mt), ";")
	return types.MediaType(strings.ToLower(strings.TrimSpace(base)))
}

// DocumentMedia are the media types Documents reads.
var DocumentMedia = []types.MediaType{types.MediaPDF, types.MediaHTML, types.MediaText, "text/markdown", types.MediaCSV}

// Documents returns the built-in extract converter for PDF, HTML and text
// documents. It uses the RAG pipeline's extractors and returns one text
// part: the document's title, then each section under its heading (one per
// page for a PDF). A document with no extractable text is an error, so a
// scanned PDF falls through to the next permitted action instead of
// reaching the model as an empty part.
func Documents() types.Converter {
	return ExtractWith("documents", "1", DocumentExtractor(), DocumentMedia...)
}

// DocumentExtractor is the types.Extractor behind Documents: the RAG
// extractor registry (rag/extractor.Auto) as a part extractor, so ingestion
// and conversion read documents the same way.
func DocumentExtractor() types.Extractor {
	return extractor.NewAuto().PartExtractor()
}
