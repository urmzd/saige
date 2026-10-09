package embedderregistry

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// DefaultMaxBatchItems is the per-request item cap used when
// WithMaxBatchItems is not set. It stays under common provider limits (for
// example 250 inputs per Gemini batch and 2048 per OpenAI request).
const DefaultMaxBatchItems = 256

// Batching is a VariantEmbedder decorator that splits large requests into
// bounded batches, sends them to the inner embedder in order, and returns the
// vectors in input order. Each batch respects an item cap and an optional
// token cap, and oversized text inputs can be truncated before they are sent.
//
// Compose decorators as cache, then batching, then retry, then the provider
// embedder, so cache hits never reach the provider and each batch is retried
// on its own. When the provider client already retries (the OpenAI SDK does
// by default), disable its retries if a retry decorator is also used, so
// attempts do not multiply.
type Batching struct {
	inner          types.VariantEmbedder
	maxItems       int
	maxTokens      int
	maxInputTokens int
	countTokens    func(string) int
}

// BatchOption configures a Batching decorator.
type BatchOption func(*Batching)

// WithMaxBatchItems caps the number of variants in one inner call. Values
// <= 0 keep DefaultMaxBatchItems.
func WithMaxBatchItems(n int) BatchOption {
	return func(b *Batching) {
		if n > 0 {
			b.maxItems = n
		}
	}
}

// WithMaxBatchTokens caps the summed text tokens of one inner call. A single
// input larger than the cap is still sent, alone in its batch. Values <= 0
// disable the token cap (the default).
func WithMaxBatchTokens(n int) BatchOption {
	return func(b *Batching) {
		if n > 0 {
			b.maxTokens = n
		}
	}
}

// WithMaxInputTokens truncates each text input to at most n tokens before it
// is sent, cutting on a UTF-8 rune boundary. Use it when the embedding model
// rejects or silently truncates longer inputs. Values <= 0 disable truncation
// (the default).
func WithMaxInputTokens(n int) BatchOption {
	return func(b *Batching) {
		if n > 0 {
			b.maxInputTokens = n
		}
	}
}

// NewBatching wraps inner with request batching.
func NewBatching(inner types.VariantEmbedder, opts ...BatchOption) *Batching {
	b := &Batching{
		inner:       inner,
		maxItems:    DefaultMaxBatchItems,
		countTokens: tokenizer.CountTokens,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Embed sends variants to the inner embedder in bounded batches and returns
// one vector per variant in input order. It fails with an error wrapping
// types.ErrEmbeddingShape when any batch returns a malformed result.
func (b *Batching) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	if len(variants) == 0 {
		return nil, nil
	}

	prepared := variants
	tokens := make([]int, len(variants))
	if b.maxInputTokens > 0 || b.maxTokens > 0 {
		prepared = make([]types.ContentVariant, len(variants))
		copy(prepared, variants)
		for i := range prepared {
			if prepared[i].Text == "" {
				continue
			}
			n := b.countTokens(prepared[i].Text)
			if b.maxInputTokens > 0 && n > b.maxInputTokens {
				prepared[i].Text = b.truncate(prepared[i].Text, b.maxInputTokens)
				n = b.countTokens(prepared[i].Text)
			}
			tokens[i] = n
		}
	}

	results := make([][]float32, 0, len(variants))
	for start := 0; start < len(prepared); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := b.batchEnd(tokens, start)
		out, err := b.inner.Embed(ctx, prepared[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		if err := types.ValidateEmbeddings(end-start, out); err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		results = append(results, out...)
		start = end
	}
	if err := types.ValidateEmbeddings(len(variants), results); err != nil {
		return nil, err
	}
	return results, nil
}

// batchEnd returns the exclusive end of the batch that begins at start. A
// batch always holds at least one variant so oversized inputs make progress.
func (b *Batching) batchEnd(tokens []int, start int) int {
	end := start
	sum := 0
	for end < len(tokens) && end-start < b.maxItems {
		if b.maxTokens > 0 && end > start && sum+tokens[end] > b.maxTokens {
			break
		}
		sum += tokens[end]
		end++
	}
	return end
}

// truncate returns the longest rune-aligned prefix of text that fits limit
// tokens.
func (b *Batching) truncate(text string, limit int) string {
	boundaries := make([]int, 0, len(text)+1)
	for i := range text {
		boundaries = append(boundaries, i)
	}
	boundaries = append(boundaries, len(text))
	lo, hi := 0, len(boundaries)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if b.countTokens(text[:boundaries[mid]]) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return text[:boundaries[lo]]
}
