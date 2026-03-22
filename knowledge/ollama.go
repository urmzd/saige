package knowledge

import (
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/knowledge/internal/extraction"
	"github.com/urmzd/saige/knowledge/types"
)

// NewOllamaExtractor creates an Extractor backed by an Ollama client.
func NewOllamaExtractor(client *ollama.Client) types.Extractor {
	return extraction.NewOllamaExtractor(client)
}

// NewOllamaEmbedder creates an Embedder backed by an Ollama client.
// This delegates to the ollama provider's NewEmbedder which implements
// the batch Embed(ctx, []string) API.
func NewOllamaEmbedder(client *ollama.Client) types.Embedder {
	return ollama.NewEmbedder(client)
}
