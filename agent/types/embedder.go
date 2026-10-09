package types

import "context"

// Embedder generates vector embeddings from text.
// Batch-first API: single embed = Embed(ctx, []string{text}).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedPurpose tells an embedder why it is embedding its input. Asymmetric
// embedding models produce different vectors for a search query and for the
// passage it should match (for example Gemini RETRIEVAL_QUERY versus
// RETRIEVAL_DOCUMENT, or e5 "query: " versus "passage: " prefixes).
type EmbedPurpose string

const (
	// PurposeUnspecified means the caller did not state a purpose. Embedders
	// should use their symmetric default.
	PurposeUnspecified EmbedPurpose = ""
	// PurposeDocument marks content that is being indexed for retrieval.
	PurposeDocument EmbedPurpose = "document"
	// PurposeQuery marks a search query that is matched against indexed content.
	PurposeQuery EmbedPurpose = "query"
)

type embedPurposeKey struct{}

// WithEmbedPurpose returns a context that carries p to every embedder called
// with it, so an embedder can select a task type without a change to its
// interface.
func WithEmbedPurpose(ctx context.Context, p EmbedPurpose) context.Context {
	return context.WithValue(ctx, embedPurposeKey{}, p)
}

// EmbedPurposeFrom returns the purpose carried by ctx, or PurposeUnspecified.
func EmbedPurposeFrom(ctx context.Context) EmbedPurpose {
	if ctx == nil {
		return PurposeUnspecified
	}
	p, _ := ctx.Value(embedPurposeKey{}).(EmbedPurpose)
	return p
}
