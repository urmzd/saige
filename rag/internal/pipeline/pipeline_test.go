package pipeline_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/graph-agent-dev-kit/rag/bm25retriever"
	"github.com/urmzd/graph-agent-dev-kit/rag/internal/pipeline"
	"github.com/urmzd/graph-agent-dev-kit/rag/memstore"
	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
	"github.com/urmzd/graph-agent-dev-kit/rag/vectorretriever"
)

type simpleExtractor struct{}

func (e *simpleExtractor) Extract(_ context.Context, raw *ragtypes.RawDocument) (*ragtypes.Document, error) {
	docUUID := "test-doc"
	secUUID := "test-sec"
	varUUID := "test-var"
	return &ragtypes.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     "Test Document",
		Sections: []ragtypes.Section{{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        0,
			Variants: []ragtypes.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: ragtypes.ContentText,
				MIMEType:    "text/plain",
				Text:        string(raw.Data),
			}},
		}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil
}

type simpleEmbedder struct{}

func (e *simpleEmbedder) Register(_ ragtypes.ContentType, _ ragtypes.VariantEmbedder) {}
func (e *simpleEmbedder) Embed(_ context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
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

func (t *trackingIndexer) Index(ctx context.Context, doc *ragtypes.Document) error {
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
		Retrievers:       []ragtypes.Retriever{tracker},
	})

	// Ingest should call Index.
	result, err := pipe.Ingest(ctx, &ragtypes.RawDocument{
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
		Retrievers:       []ragtypes.Retriever{vecRetriever, bm25},
	})

	_, err := pipe.Ingest(ctx, &ragtypes.RawDocument{
		SourceURI: "test://doc",
		Data:      []byte("the quick brown fox jumps over the lazy dog"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Search should combine results from both retrievers via RRF.
	sr, err := pipe.Search(ctx, "quick brown fox", ragtypes.WithLimit(5))
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

func (e *uniqueExtractor) Extract(_ context.Context, raw *ragtypes.RawDocument) (*ragtypes.Document, error) {
	n := e.counter.Add(1)
	docUUID := fmt.Sprintf("doc-%d", n)
	secUUID := fmt.Sprintf("sec-%d", n)
	varUUID := fmt.Sprintf("var-%d", n)
	return &ragtypes.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     "Test Document",
		Sections: []ragtypes.Section{{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        0,
			Variants: []ragtypes.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: ragtypes.ContentText,
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
		DedupBehavior:    ragtypes.DedupSkip,
	})

	raw := &ragtypes.RawDocument{
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
		DedupBehavior:    ragtypes.DedupReplace,
	})

	raw := &ragtypes.RawDocument{
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
	if err != ragtypes.ErrDocumentNotFound {
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

	raw1 := &ragtypes.RawDocument{
		SourceURI: "test://update",
		Data:      []byte("original content"),
	}

	r1, err := pipe.Ingest(ctx, raw1)
	if err != nil {
		t.Fatal(err)
	}

	raw2 := &ragtypes.RawDocument{
		SourceURI: "test://update",
		Data:      []byte("updated content"),
	}

	r2, err := pipe.Update(ctx, r1.DocumentUUID, raw2)
	if err != nil {
		t.Fatal(err)
	}

	// Old document should be deleted.
	_, err = store.GetDocument(ctx, r1.DocumentUUID)
	if err != ragtypes.ErrDocumentNotFound {
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

	raw := &ragtypes.RawDocument{
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
