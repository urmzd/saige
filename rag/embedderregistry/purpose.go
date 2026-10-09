package embedderregistry

import (
	"context"

	"github.com/urmzd/saige/rag/types"
)

// PurposeRouter is a VariantEmbedder that sends search queries to one
// embedder and everything else to another. Use it with asymmetric models
// exposed as two configured clients, for example a Gemini embedder with task
// type RETRIEVAL_QUERY and one with RETRIEVAL_DOCUMENT. The purpose comes
// from types.EmbedPurposeFrom: the pipeline marks ingest as PurposeDocument
// and the vector retriever marks queries as PurposeQuery. Both embedders
// must produce vectors of the same dimension.
type PurposeRouter struct {
	document types.VariantEmbedder
	query    types.VariantEmbedder
}

// NewPurposeRouter returns a router. A nil query embedder falls back to the
// document embedder.
func NewPurposeRouter(document, query types.VariantEmbedder) *PurposeRouter {
	if query == nil {
		query = document
	}
	return &PurposeRouter{document: document, query: query}
}

// Embed implements types.VariantEmbedder.
func (r *PurposeRouter) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	if types.EmbedPurposeFrom(ctx) == types.PurposeQuery {
		return r.query.Embed(ctx, variants)
	}
	return r.document.Embed(ctx, variants)
}

// Prefixed is a VariantEmbedder that prepends a purpose-specific instruction
// to the text of each text variant before calling the inner embedder, as
// models such as e5 ("query: " and "passage: ") and nomic-embed
// ("search_query: " and "search_document: ") expect. PurposeUnspecified uses
// the document prefix. Non-text variants pass through unchanged, and the
// caller's variants are never modified.
type Prefixed struct {
	inner          types.VariantEmbedder
	queryPrefix    string
	documentPrefix string
}

// NewPrefixed returns a Prefixed embedder.
func NewPrefixed(inner types.VariantEmbedder, queryPrefix, documentPrefix string) *Prefixed {
	return &Prefixed{inner: inner, queryPrefix: queryPrefix, documentPrefix: documentPrefix}
}

// Embed implements types.VariantEmbedder.
func (p *Prefixed) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	prefix := p.documentPrefix
	if types.EmbedPurposeFrom(ctx) == types.PurposeQuery {
		prefix = p.queryPrefix
	}
	if prefix == "" {
		return p.inner.Embed(ctx, variants)
	}
	prefixed := make([]types.ContentVariant, len(variants))
	for i, v := range variants {
		if v.ContentType == types.ContentText {
			v.Text = prefix + v.Text
		}
		prefixed[i] = v
	}
	return p.inner.Embed(ctx, prefixed)
}
