package embedderregistry

import (
	"context"

	"github.com/urmzd/saige/rag/types"
)

// TextEmbedder embeds strings. Every adapter's embedder implements it, for
// example ollama.NewEmbedder, openai.NewEmbedder and google.NewEmbedder.
type TextEmbedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Text adapts a TextEmbedder to a types.VariantEmbedder that embeds the
// Text of each content variant, in order:
//
//	rag.WithEmbedders(embedderregistry.NewTextOnly(embedderregistry.Text(emb)))
func Text(e TextEmbedder) types.VariantEmbedder { return textEmbedder{e} }

type textEmbedder struct{ e TextEmbedder }

func (t textEmbedder) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	texts := make([]string, len(variants))
	for i, v := range variants {
		texts[i] = v.Text
	}
	return t.e.Embed(ctx, texts)
}
