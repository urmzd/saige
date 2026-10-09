package google

import (
	"context"
	"net/http"

	"github.com/urmzd/saige/agent/types"
	"golang.org/x/sync/errgroup"
	"google.golang.org/genai"
)

// Embedder implements types.Embedder using the official Google GenAI SDK.
type Embedder struct {
	client     *genai.Client
	model      string
	taskType   string
	httpClient *http.Client
}

// EmbedderOption configures an Embedder.
type EmbedderOption func(*Embedder)

// WithTaskType fixes the task type sent with every request, for example
// "SEMANTIC_SIMILARITY" or "CLUSTERING", in place of the one chosen from the
// context's embed purpose.
func WithTaskType(taskType string) EmbedderOption {
	return func(e *Embedder) { e.taskType = taskType }
}

// WithEmbedHTTPClient sets the HTTP client the embedder's SDK client uses.
func WithEmbedHTTPClient(h *http.Client) EmbedderOption {
	return func(e *Embedder) { e.httpClient = h }
}

// NewEmbedder creates a new Google embedder.
func NewEmbedder(ctx context.Context, apiKey, model string, opts ...EmbedderOption) (*Embedder, error) {
	e := &Embedder{model: model}
	for _, o := range opts {
		o(e)
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: withHeaderTransport(e.httpClient),
	})
	if err != nil {
		return nil, err
	}
	e.client = client
	return e, nil
}

// taskTypeFor picks the task type for a request: the fixed one when set,
// otherwise RETRIEVAL_QUERY or RETRIEVAL_DOCUMENT from the embed purpose
// carried by ctx (types.WithEmbedPurpose). An unstated purpose sends none, so
// the model's symmetric default applies.
func (e *Embedder) taskTypeFor(ctx context.Context) string {
	if e.taskType != "" {
		return e.taskType
	}
	switch types.EmbedPurposeFrom(ctx) {
	case types.PurposeQuery:
		return "RETRIEVAL_QUERY"
	case types.PurposeDocument:
		return "RETRIEVAL_DOCUMENT"
	}
	return ""
}

// Embed implements types.Embedder with parallel API calls. Failures are
// classified like chat errors, so a rate limit or an overload is transient
// to a retrying caller.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	embeddings := make([][]float32, len(texts))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(10)

	var cfg *genai.EmbedContentConfig
	if tt := e.taskTypeFor(ctx); tt != "" {
		cfg = &genai.EmbedContentConfig{TaskType: tt}
	}
	for i, text := range texts {
		g.Go(func() error {
			callCtx, sink := withRetryAfterSink(gctx)
			resp, err := e.client.Models.EmbedContent(callCtx, e.model, genai.Text(text), cfg)
			if err != nil {
				return classifyWithHeader(e.model, err, true, sink)
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
