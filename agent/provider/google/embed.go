package google

import (
	"context"

	"golang.org/x/sync/errgroup"
	"google.golang.org/genai"
)

// Embedder implements core.Embedder using the official Google GenAI SDK.
type Embedder struct {
	client *genai.Client
	model  string
}

// NewEmbedder creates a new Google embedder.
func NewEmbedder(ctx context.Context, apiKey, model string) (*Embedder, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &Embedder{client: client, model: model}, nil
}

// Embed implements core.Embedder with parallel API calls.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	embeddings := make([][]float32, len(texts))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(10)

	for i, text := range texts {
		g.Go(func() error {
			resp, err := e.client.Models.EmbedContent(gctx, e.model, genai.Text(text), nil)
			if err != nil {
				return err
			}
			if len(resp.Embeddings) > 0 {
				embeddings[i] = resp.Embeddings[0].Values
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return embeddings, nil
}
