package embeddingcache_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/rag/embeddingcache"
	"github.com/urmzd/saige/rag/types"
)

type countingEmbedder struct {
	callCount   atomic.Int32
	variantsSum atomic.Int32
}

func (e *countingEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	e.callCount.Add(1)
	e.variantsSum.Add(int32(len(variants)))
	result := make([][]float32, len(variants))
	for i, v := range variants {
		result[i] = []float32{float32(len(v.Text))}
	}
	return result, nil
}

func TestCacheHit(t *testing.T) {
	inner := &countingEmbedder{}
	cache := embeddingcache.New(inner)

	variants := []types.ContentVariant{
		{UUID: "v1", ContentType: types.ContentText, Text: "hello"},
	}

	// First call: miss.
	r1, err := cache.Embed(context.Background(), variants)
	if err != nil {
		t.Fatal(err)
	}

	// Second call: hit.
	r2, err := cache.Embed(context.Background(), variants)
	if err != nil {
		t.Fatal(err)
	}

	if inner.callCount.Load() != 1 {
		t.Errorf("expected 1 inner call, got %d", inner.callCount.Load())
	}

	if r1[0][0] != r2[0][0] {
		t.Errorf("cached result should match: %v vs %v", r1[0], r2[0])
	}
}

func TestCacheMiss(t *testing.T) {
	inner := &countingEmbedder{}
	cache := embeddingcache.New(inner)

	v1 := []types.ContentVariant{{UUID: "v1", ContentType: types.ContentText, Text: "hello"}}
	v2 := []types.ContentVariant{{UUID: "v2", ContentType: types.ContentText, Text: "world"}}

	if _, err := cache.Embed(context.Background(), v1); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Embed(context.Background(), v2); err != nil {
		t.Fatal(err)
	}

	if inner.callCount.Load() != 2 {
		t.Errorf("expected 2 inner calls for different texts, got %d", inner.callCount.Load())
	}
}

func TestLRUEviction(t *testing.T) {
	inner := &countingEmbedder{}
	cache := embeddingcache.New(inner, embeddingcache.WithMaxSize(2))

	ctx := context.Background()
	texts := []string{"aaa", "bbb", "ccc"}

	for _, text := range texts {
		v := []types.ContentVariant{{ContentType: types.ContentText, Text: text}}
		if _, err := cache.Embed(ctx, v); err != nil {
			t.Fatal(err)
		}
	}

	// 3 unique texts, 3 inner calls so far.
	if inner.callCount.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", inner.callCount.Load())
	}

	// "aaa" should have been evicted (oldest). Re-embedding it should trigger a new call.
	v := []types.ContentVariant{{ContentType: types.ContentText, Text: "aaa"}}
	if _, err := cache.Embed(ctx, v); err != nil {
		t.Fatal(err)
	}

	if inner.callCount.Load() != 4 {
		t.Errorf("expected 4 calls after eviction, got %d", inner.callCount.Load())
	}

	// "ccc" should still be cached.
	v = []types.ContentVariant{{ContentType: types.ContentText, Text: "ccc"}}
	if _, err := cache.Embed(ctx, v); err != nil {
		t.Fatal(err)
	}

	if inner.callCount.Load() != 4 {
		t.Errorf("expected 4 calls (ccc cached), got %d", inner.callCount.Load())
	}
}

func TestMixedHitMiss(t *testing.T) {
	inner := &countingEmbedder{}
	cache := embeddingcache.New(inner)

	ctx := context.Background()

	// Pre-populate cache with "hello".
	v := []types.ContentVariant{{ContentType: types.ContentText, Text: "hello"}}
	if _, err := cache.Embed(ctx, v); err != nil {
		t.Fatal(err)
	}

	// Now embed a batch with one hit and one miss.
	batch := []types.ContentVariant{
		{ContentType: types.ContentText, Text: "hello"}, // hit
		{ContentType: types.ContentText, Text: "world"}, // miss
	}
	results, err := cache.Embed(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Inner should have been called twice total: once for initial "hello", once for "world".
	if inner.callCount.Load() != 2 {
		t.Errorf("expected 2 inner calls, got %d", inner.callCount.Load())
	}

	// The miss batch should have had only 1 variant.
	if inner.variantsSum.Load() != 2 {
		t.Errorf("expected 2 total variants embedded, got %d", inner.variantsSum.Load())
	}
}

func TestCacheEmpty(t *testing.T) {
	inner := &countingEmbedder{}
	cache := embeddingcache.New(inner)

	results, err := cache.Embed(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Errorf("expected nil for empty input, got %v", results)
	}
	if inner.callCount.Load() != 0 {
		t.Errorf("expected 0 inner calls for empty input, got %d", inner.callCount.Load())
	}
}

func TestBinaryInputsAndReturnedVectorsAreIndependent(t *testing.T) {
	inner := &countingEmbedder{}
	cache := embeddingcache.New(inner)
	first := types.ContentVariant{ContentType: types.ContentImage, MIMEType: "image/png", Data: []byte{1}, Text: "same"}
	second := first
	second.Data = []byte{2}
	for _, variant := range []types.ContentVariant{first, second} {
		if _, err := cache.Embed(context.Background(), []types.ContentVariant{variant}); err != nil {
			t.Fatal(err)
		}
	}
	second = first
	second.MIMEType = "image/jpeg"
	if _, err := cache.Embed(context.Background(), []types.ContentVariant{second}); err != nil {
		t.Fatal(err)
	}
	if inner.callCount.Load() != 3 {
		t.Fatal("binary or MIME identity collapsed")
	}
	values, err := cache.Embed(context.Background(), []types.ContentVariant{first})
	if err != nil {
		t.Fatal(err)
	}
	values[0][0] = 99
	again, err := cache.Embed(context.Background(), []types.ContentVariant{first})
	if err != nil {
		t.Fatal(err)
	}
	if again[0][0] == 99 {
		t.Fatal("reader poisoned vector")
	}
}

type shortEmbedder struct{}

func (shortEmbedder) Embed(context.Context, []types.ContentVariant) ([][]float32, error) {
	return nil, nil
}
func TestMissingEmbeddingFails(t *testing.T) {
	_, err := embeddingcache.New(shortEmbedder{}).Embed(context.Background(), []types.ContentVariant{{Text: "test"}})
	if err == nil {
		t.Fatal("missing vector reported success")
	}
}
