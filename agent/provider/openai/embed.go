package openai

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/types"
)

// Embedder implements types.Embedder using the official OpenAI SDK.
type Embedder struct {
	client openai.Client
	model  openai.EmbeddingModel
}

// NewEmbedder creates a new OpenAI embedder. Unlike the chat adapter it keeps
// the SDK's built-in retries unless WithMaxRetries says otherwise; pass
// WithMaxRetries(0) when a retry decorator (embedderregistry.NewRetrying)
// wraps it, so attempts are not multiplied.
//
// Embed sends all texts in one request, and the API limits how many inputs
// and tokens one request may carry. Wrap the embedder with
// embedderregistry.NewBatching to split large inputs. OpenAI embedding
// models are symmetric, so the embed purpose carried by the context does not
// change the request.
//
// A missing model is an error wrapping types.ErrInvalidConfig.
func NewEmbedder(cfg Config, opts ...Option) (*Embedder, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("%w: openai: embedder Config.Model is required", types.ErrInvalidConfig)
	}
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return &Embedder{
		client: openai.NewClient(c.clientOptions(cfg.APIKey, nil)...),
		model:  openai.EmbeddingModel(cfg.Model),
	}, nil
}

// Embed implements types.Embedder.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	resp, err := e.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: texts,
		},
		Model: e.model,
	})
	if err != nil {
		return nil, classifyOpenAIError(string(e.model), err, true)
	}

	embeddings := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if int(d.Index) < len(embeddings) {
			f32 := make([]float32, len(d.Embedding))
			for j, v := range d.Embedding {
				f32[j] = float32(v)
			}
			embeddings[d.Index] = f32
		}
	}
	return embeddings, nil
}
