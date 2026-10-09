package embedderregistry_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// shapeEmbedder returns a fixed, possibly malformed, result.
type shapeEmbedder struct {
	out func(n int) [][]float32
}

func (s shapeEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	return s.out(len(variants)), nil
}

func vectors(n, dim int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		out[i] = make([]float32, dim)
		out[i][0] = 1
	}
	return out
}

func TestRegistryRejectsMalformedEmbeddings(t *testing.T) {
	tests := []struct {
		name string
		out  func(n int) [][]float32
	}{
		{name: "one vector short", out: func(n int) [][]float32 { return vectors(n-1, 3) }},
		{name: "one vector extra", out: func(n int) [][]float32 { return vectors(n+1, 3) }},
		{name: "nil vector", out: func(n int) [][]float32 { v := vectors(n, 3); v[1] = nil; return v }},
		{name: "mixed dimensions", out: func(n int) [][]float32 { v := vectors(n, 3); v[1] = []float32{1, 2}; return v }},
	}
	variants := []types.ContentVariant{
		{ContentType: types.ContentText, Text: "a"},
		{ContentType: types.ContentText, Text: "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := embedderregistry.NewTextOnly(shapeEmbedder{out: tt.out})
			_, err := r.Embed(context.Background(), variants)
			if !errors.Is(err, types.ErrEmbeddingShape) {
				t.Fatalf("expected ErrEmbeddingShape, got %v", err)
			}
		})
	}
}

func TestRegistryAllowsDifferentDimensionsAcrossEmbedders(t *testing.T) {
	r := embedderregistry.New()
	r.Register(types.ContentText, shapeEmbedder{out: func(n int) [][]float32 { return vectors(n, 3) }})
	r.Register(types.ContentImage, shapeEmbedder{out: func(n int) [][]float32 { return vectors(n, 5) }})
	out, err := r.Embed(context.Background(), []types.ContentVariant{
		{ContentType: types.ContentText, Text: "a"},
		{ContentType: types.ContentImage},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0]) != 3 || len(out[1]) != 5 {
		t.Errorf("unexpected dimensions %d and %d", len(out[0]), len(out[1]))
	}
}

// recordingEmbedder records every batch and returns a vector whose first
// element is the input's position in the original request.
type recordingEmbedder struct {
	mu      sync.Mutex
	batches [][]types.ContentVariant
}

func (r *recordingEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]types.ContentVariant(nil), variants...))
	r.mu.Unlock()
	out := make([][]float32, len(variants))
	for i, v := range variants {
		var pos int
		fmt.Sscanf(v.UUID, "v%d", &pos)
		out[i] = []float32{float32(pos)}
	}
	return out, nil
}

func makeVariants(n int, text string) []types.ContentVariant {
	out := make([]types.ContentVariant, n)
	for i := range out {
		out[i] = types.ContentVariant{UUID: fmt.Sprintf("v%d", i), ContentType: types.ContentText, Text: text}
	}
	return out
}

func TestBatchingSplitsAndPreservesOrder(t *testing.T) {
	tests := []struct {
		name        string
		n           int
		opts        []embedderregistry.BatchOption
		text        string
		wantBatches int
	}{
		{name: "5000 items at 256", n: 5000, opts: []embedderregistry.BatchOption{embedderregistry.WithMaxBatchItems(256)}, text: "x", wantBatches: 20},
		{name: "default cap", n: 300, text: "x", wantBatches: 2},
		{name: "single batch", n: 10, opts: []embedderregistry.BatchOption{embedderregistry.WithMaxBatchItems(256)}, text: "x", wantBatches: 1},
		{name: "token cap", n: 10, opts: []embedderregistry.BatchOption{embedderregistry.WithMaxBatchTokens(2 * tokenizer.CountTokens("hello world"))}, text: "hello world", wantBatches: 5},
		{name: "oversized input alone", n: 3, opts: []embedderregistry.BatchOption{embedderregistry.WithMaxBatchTokens(1)}, text: "hello world again", wantBatches: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &recordingEmbedder{}
			b := embedderregistry.NewBatching(inner, tt.opts...)
			out, err := b.Embed(context.Background(), makeVariants(tt.n, tt.text))
			if err != nil {
				t.Fatal(err)
			}
			if len(inner.batches) != tt.wantBatches {
				t.Errorf("got %d inner calls, want %d", len(inner.batches), tt.wantBatches)
			}
			if len(out) != tt.n {
				t.Fatalf("got %d vectors, want %d", len(out), tt.n)
			}
			for i, vec := range out {
				if int(vec[0]) != i {
					t.Fatalf("vector %d belongs to input %d", i, int(vec[0]))
				}
			}
		})
	}
}

func TestBatchingTruncatesOversizedInputs(t *testing.T) {
	inner := &recordingEmbedder{}
	b := embedderregistry.NewBatching(inner, embedderregistry.WithMaxInputTokens(8))
	long := strings.Repeat("héllo wörld ", 50)
	variants := makeVariants(2, long)
	if _, err := b.Embed(context.Background(), variants); err != nil {
		t.Fatal(err)
	}
	sent := inner.batches[0][0].Text
	if n := tokenizer.CountTokens(sent); n > 8 || n == 0 {
		t.Errorf("sent text has %d tokens, want 1..8", n)
	}
	if !utf8.ValidString(sent) || !strings.HasPrefix(long, sent) {
		t.Errorf("truncated text %q is not a valid prefix", sent)
	}
	if variants[0].Text != long {
		t.Error("caller's variant was modified")
	}
}

func TestBatchingRejectsMalformedBatch(t *testing.T) {
	b := embedderregistry.NewBatching(shapeEmbedder{out: func(n int) [][]float32 { return vectors(n-1, 2) }},
		embedderregistry.WithMaxBatchItems(2))
	_, err := b.Embed(context.Background(), makeVariants(5, "x"))
	if !errors.Is(err, types.ErrEmbeddingShape) {
		t.Fatalf("expected ErrEmbeddingShape, got %v", err)
	}
}

func TestBatchingStopsOnCancelledContext(t *testing.T) {
	inner := &recordingEmbedder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := embedderregistry.NewBatching(inner).Embed(ctx, makeVariants(3, "x"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(inner.batches) != 0 {
		t.Errorf("inner embedder called %d times after cancel", len(inner.batches))
	}
}
