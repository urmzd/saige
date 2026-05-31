package pipeline_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

type simpleExtractor struct{}

func (e *simpleExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	docUUID := "test-doc"
	secUUID := "test-sec"
	varUUID := "test-var"
	return &types.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     "Test Document",
		Sections: []types.Section{{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        0,
			Variants: []types.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: types.ContentText,
				MIMEType:    "text/plain",
				Text:        string(raw.Data),
			}},
		}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil
}

type simpleEmbedder struct{}

func (e *simpleEmbedder) Register(_ types.ContentType, _ types.VariantEmbedder) {}
func (e *simpleEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	result := make([][]float32, len(variants))
	for i := range variants {
		// Simple hash-based embedding for testing.
		vec := make([]float32, 4)
		text := variants[i].Text
		for j, ch := range text {
			vec[j%4] += float32(ch)
		}
		result[i] = vec
	}
	return result, nil
}

// trackingIndexer wraps BM25 retriever and tracks calls.
type trackingIndexer struct {
	*bm25retriever.Retriever
	indexCalled  bool
	removeCalled bool
}

func (t *trackingIndexer) Index(ctx context.Context, doc *types.Document) error {
	t.indexCalled = true
	return t.Retriever.Index(ctx, doc)
}

func (t *trackingIndexer) Remove(ctx context.Context, docUUID string) error {
	t.removeCalled = true
	return t.Retriever.Remove(ctx, docUUID)
}

func TestPipelineIndexerIntegration(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	bm25 := bm25retriever.New(store, nil)
	tracker := &trackingIndexer{Retriever: bm25}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{tracker},
	})

	// Ingest should call Index.
	result, err := pipe.Ingest(ctx, &types.RawDocument{
		SourceURI: "test://doc",
		Data:      []byte("the quick brown fox jumps over the lazy dog"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.indexCalled {
		t.Error("expected Indexer.Index to be called during ingest")
	}

	// Delete should call Remove.
	err = pipe.Delete(ctx, result.DocumentUUID)
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.removeCalled {
		t.Error("expected Indexer.Remove to be called during delete")
	}
}

func TestPipelineHybridSearch(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	embedders := &simpleEmbedder{}

	bm25 := bm25retriever.New(store, nil)
	vecRetriever := vectorretriever.New(store, embedders)

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Embedders:        embedders,
		Retrievers:       []types.Retriever{vecRetriever, bm25},
	})

	_, err := pipe.Ingest(ctx, &types.RawDocument{
		SourceURI: "test://doc",
		Data:      []byte("the quick brown fox jumps over the lazy dog"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Search should combine results from both retrievers via RRF.
	sr, err := pipe.Search(ctx, "quick brown fox", types.WithLimit(5))
	if err != nil {
		t.Fatal(err)
	}

	if len(sr.Hits) == 0 {
		t.Error("expected at least one hit from hybrid search")
	}
}

// uniqueExtractor generates unique UUIDs per extraction for dedup testing.
type uniqueExtractor struct {
	counter atomic.Int32
}

func (e *uniqueExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	n := e.counter.Add(1)
	docUUID := fmt.Sprintf("doc-%d", n)
	secUUID := fmt.Sprintf("sec-%d", n)
	varUUID := fmt.Sprintf("var-%d", n)
	return &types.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     "Test Document",
		Sections: []types.Section{{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        0,
			Variants: []types.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: types.ContentText,
				MIMEType:    "text/plain",
				Text:        string(raw.Data),
			}},
		}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil
}

func TestPipelineDedupSkip(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	extractor := &uniqueExtractor{}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: extractor,
		DedupBehavior:    types.DedupSkip,
	})

	raw := &types.RawDocument{
		SourceURI: "test://dup",
		Data:      []byte("duplicate content"),
	}

	// First ingest.
	r1, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Deduplicated {
		t.Error("first ingest should not be deduplicated")
	}

	// Second ingest with same data should be skipped.
	r2, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Deduplicated {
		t.Error("second ingest should be deduplicated with DedupSkip")
	}
	if r2.DocumentUUID != r1.DocumentUUID {
		t.Errorf("dedup should return same UUID: got %s, want %s", r2.DocumentUUID, r1.DocumentUUID)
	}
}

func TestPipelineDedupReplace(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	extractor := &uniqueExtractor{}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: extractor,
		DedupBehavior:    types.DedupReplace,
	})

	raw := &types.RawDocument{
		SourceURI: "test://dup",
		Data:      []byte("duplicate content for replace"),
	}

	// First ingest.
	r1, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Second ingest with same data should replace.
	r2, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Old document should be gone.
	_, err = store.GetDocument(ctx, r1.DocumentUUID)
	if err != types.ErrDocumentNotFound {
		t.Errorf("old document should be deleted, got err: %v", err)
	}

	// New document should exist.
	_, err = store.GetDocument(ctx, r2.DocumentUUID)
	if err != nil {
		t.Errorf("new document should exist: %v", err)
	}
}

func TestPipelineUpdate(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	extractor := &uniqueExtractor{}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: extractor,
	})

	raw1 := &types.RawDocument{
		SourceURI: "test://update",
		Data:      []byte("original content"),
	}

	r1, err := pipe.Ingest(ctx, raw1)
	if err != nil {
		t.Fatal(err)
	}

	raw2 := &types.RawDocument{
		SourceURI: "test://update",
		Data:      []byte("updated content"),
	}

	r2, err := pipe.Update(ctx, r1.DocumentUUID, raw2)
	if err != nil {
		t.Fatal(err)
	}

	// Old document should be deleted.
	_, err = store.GetDocument(ctx, r1.DocumentUUID)
	if err != types.ErrDocumentNotFound {
		t.Errorf("old document should be deleted after update, got err: %v", err)
	}

	// New document should exist.
	doc, err := store.GetDocument(ctx, r2.DocumentUUID)
	if err != nil {
		t.Fatalf("new document should exist: %v", err)
	}
	if doc.Sections[0].Variants[0].Text != "updated content" {
		t.Errorf("expected updated content, got %q", doc.Sections[0].Variants[0].Text)
	}
}

func TestPipelineReconstruct(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
	})

	raw := &types.RawDocument{
		SourceURI: "test://reconstruct",
		Data:      []byte("document for reconstruction"),
	}

	result, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	doc, err := pipe.Reconstruct(ctx, result.DocumentUUID)
	if err != nil {
		t.Fatal(err)
	}

	if doc.UUID != result.DocumentUUID {
		t.Errorf("expected UUID %s, got %s", result.DocumentUUID, doc.UUID)
	}
	if len(doc.Sections) == 0 {
		t.Error("expected at least one section")
	}
	if doc.Sections[0].Variants[0].Text != "document for reconstruction" {
		t.Errorf("unexpected text: %q", doc.Sections[0].Variants[0].Text)
	}
}

// recencyEmbedder returns a fixed embedding regardless of input so that two
// documents are exactly equally relevant to any query.
type recencyEmbedder struct{}

func (e *recencyEmbedder) Register(_ types.ContentType, _ types.VariantEmbedder) {}
func (e *recencyEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(variants))
	for i := range variants {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// seedRecencyDocs inserts two documents with identical embeddings (equal
// relevance) but different UpdatedAt ages: "recent-var" and "old-var".
func seedRecencyDocs(t *testing.T, store *memstore.Store, now time.Time) {
	t.Helper()
	recent := &types.Document{
		UUID:      "doc-recent",
		Title:     "Recent",
		UpdatedAt: now.Add(-1 * time.Hour),
		CreatedAt: now.Add(-1 * time.Hour),
		Sections: []types.Section{{
			UUID: "sec-recent", DocumentUUID: "doc-recent", Index: 0,
			Variants: []types.ContentVariant{{
				UUID: "recent-var", SectionUUID: "sec-recent",
				ContentType: types.ContentText, Text: "answer", Embedding: []float32{1, 0, 0},
			}},
		}},
	}
	old := &types.Document{
		UUID:      "doc-old",
		Title:     "Old",
		UpdatedAt: now.Add(-365 * 24 * time.Hour),
		CreatedAt: now.Add(-365 * 24 * time.Hour),
		Sections: []types.Section{{
			UUID: "sec-old", DocumentUUID: "doc-old", Index: 0,
			Variants: []types.ContentVariant{{
				UUID: "old-var", SectionUUID: "sec-old",
				ContentType: types.ContentText, Text: "answer", Embedding: []float32{1, 0, 0},
			}},
		}},
	}
	if err := store.CreateDocument(context.Background(), recent); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDocument(context.Background(), old); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRecencyTieBreak(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	store := memstore.New()
	seedRecencyDocs(t, store, now)

	embedders := &recencyEmbedder{}
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Embedders:        embedders,
		Retrievers:       []types.Retriever{vectorretriever.New(store, embedders)},
	})

	// Baseline: both equally-relevant docs are returned. Their RRF scores are
	// near-identical (cosine ties; rank order decides the tiny remainder), so
	// recency is what should decisively reorder them below.
	base, err := pipe.Search(ctx, "answer", types.WithLimit(5))
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Hits) != 2 {
		t.Fatalf("baseline: expected 2 hits, got %d", len(base.Hits))
	}
	baseScores := map[string]float64{}
	for _, h := range base.Hits {
		baseScores[h.Variant.UUID] = h.Score
	}
	// Sanity: the two scores are within a hair of each other (no recency tilt).
	if d := baseScores["recent-var"] - baseScores["old-var"]; d > 0.001 || d < -0.001 {
		t.Fatalf("baseline: expected near-equal scores, got %v vs %v",
			baseScores["recent-var"], baseScores["old-var"])
	}

	// With recency, the recent doc must outrank the older one. A 30-day
	// half-life with weight 1.0 decays the year-old doc by ~exp(-ln2*365/30),
	// far below the recent (1h-old) doc, dominating the tiny RRF rank gap.
	withRec, err := pipe.Search(ctx, "answer",
		types.WithLimit(5),
		types.WithRecency(30*24*time.Hour, 1.0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(withRec.Hits) != 2 {
		t.Fatalf("recency: expected 2 hits, got %d", len(withRec.Hits))
	}
	if withRec.Hits[0].Variant.UUID != "recent-var" {
		t.Errorf("recency: expected recent-var first, got %q", withRec.Hits[0].Variant.UUID)
	}
	if withRec.Hits[1].Variant.UUID != "old-var" {
		t.Errorf("recency: expected old-var second, got %q", withRec.Hits[1].Variant.UUID)
	}
	if withRec.Hits[0].Score <= withRec.Hits[1].Score {
		t.Errorf("recency: recent score %v should exceed old score %v",
			withRec.Hits[0].Score, withRec.Hits[1].Score)
	}
}

func TestPipelineRecencyDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	store := memstore.New()
	seedRecencyDocs(t, store, now)

	embedders := &recencyEmbedder{}
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Embedders:        embedders,
		Retrievers:       []types.Retriever{vectorretriever.New(store, embedders)},
	})

	// A zero/negative half-life is a no-op even if the option is supplied: the
	// year-old doc must NOT be penalized, so its score stays near the recent
	// doc's (the only difference being the tiny RRF rank remainder).
	res, err := pipe.Search(ctx, "answer", types.WithRecency(0, 0.9))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(res.Hits))
	}
	scores := map[string]float64{}
	for _, h := range res.Hits {
		scores[h.Variant.UUID] = h.Score
	}
	if d := scores["recent-var"] - scores["old-var"]; d > 0.001 || d < -0.001 {
		t.Errorf("recency disabled: expected near-equal scores, got %v vs %v",
			scores["recent-var"], scores["old-var"])
	}
}

func TestPipelineReconstructNotFound(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
	})

	_, err := pipe.Reconstruct(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent document")
	}
}
