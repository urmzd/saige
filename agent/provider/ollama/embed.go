package ollama

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// OllamaEmbedder implements core.Embedder using the Ollama API.
type OllamaEmbedder struct {
	Client *Client
}

// NewEmbedder creates a new OllamaEmbedder.
func NewEmbedder(client *Client) *OllamaEmbedder {
	return &OllamaEmbedder{Client: client}
}

// Embed implements core.Embedder with parallel API calls.
func (e *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)

	for i, text := range texts {
		g.Go(func() error {
			vec, err := e.Client.Embed(gctx, text)
			if err != nil {
				return err
			}
			results[i] = vec
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
