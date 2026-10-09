package pipeline

import (
	"errors"

	ragtypes "github.com/urmzd/saige/rag/types"
)

// endSpan records err on span, if any, and ends it.
func endSpan(span ragtypes.Span, err error) {
	if err != nil {
		span.RecordError(err)
	}
	span.End()
}

// endIngestSpan annotates an ingest span with its outcome and ends it. A
// partial ingest is marked partial and still records its error.
func endIngestSpan(span ragtypes.Span, result *ragtypes.IngestResult, err error) {
	if result != nil {
		span.SetAttributes(
			ragtypes.Attr(ragtypes.AttrDocumentUUID, result.DocumentUUID),
			ragtypes.Attr(ragtypes.AttrDeduplicated, result.Deduplicated),
			ragtypes.Attr(ragtypes.AttrSections, result.Sections),
			ragtypes.Attr(ragtypes.AttrVariants, result.Variants),
		)
	}
	if errors.Is(err, ragtypes.ErrPartialIngest) {
		span.SetAttributes(ragtypes.Attr(ragtypes.AttrPartial, true))
	}
	endSpan(span, err)
}

// componentName returns v's name when it implements ragtypes.Named and
// reports a non-empty one, and fallback otherwise.
func componentName(v any, fallback string) string {
	if n, ok := v.(ragtypes.Named); ok {
		if name := n.Name(); name != "" {
			return name
		}
	}
	return fallback
}
