package embedderregistry

import (
	"context"
	"time"

	"github.com/urmzd/saige/rag/types"
)

// Observed is a VariantEmbedder decorator that opens a types.SpanEmbed span
// around each call and reports it to Observer.RecordEmbedding under its
// name. Wrap each provider embedder to see per-embedder latency, input
// counts, and failures.
type Observed struct {
	inner    types.VariantEmbedder
	name     string
	observer types.Observer
}

// NewObserved wraps inner. A nil observer records nothing.
func NewObserved(inner types.VariantEmbedder, name string, observer types.Observer) *Observed {
	if observer == nil {
		observer = types.NoopObserver{}
	}
	return &Observed{inner: inner, name: name, observer: observer}
}

// Name implements types.Named.
func (o *Observed) Name() string { return o.name }

// Unwrap returns the inner embedder.
func (o *Observed) Unwrap() types.VariantEmbedder { return o.inner }

// Embed implements types.VariantEmbedder.
func (o *Observed) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	ctx, span := o.observer.StartSpan(ctx, types.SpanEmbed,
		types.Attr(types.AttrEmbedder, o.name),
		types.Attr(types.AttrInputs, len(variants)))
	start := time.Now()
	out, err := o.inner.Embed(ctx, variants)
	elapsed := time.Since(start)
	if err != nil {
		span.RecordError(err)
	}
	span.End()
	o.observer.RecordEmbedding(ctx, types.EmbeddingRecord{
		Embedder: o.name,
		Purpose:  types.EmbedPurposeFrom(ctx),
		Inputs:   len(variants),
		Duration: elapsed,
		Err:      err,
	})
	return out, err
}
