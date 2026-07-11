package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	knowledgetypes "github.com/urmzd/saige/knowledge/types"
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

// failingEmbedder fails after allowing failAfter successful Embed calls.
type failingEmbedder struct {
	calls     atomic.Int32
	failAfter int32
}

func (e *failingEmbedder) Register(_ types.ContentType, _ types.VariantEmbedder) {}
func (e *failingEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	if e.calls.Add(1) > e.failAfter {
		return nil, errors.New("embedder unavailable")
	}
	out := make([][]float32, len(variants))
	for i := range variants {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// TestPipelineReplaceFailureKeepsPriorDocument verifies that with
// DedupReplace, a mid-ingest failure (embedder) leaves the previously
// committed document fully retrievable: the delete no longer happens before
// the fallible stages.
func TestPipelineReplaceFailureKeepsPriorDocument(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	embedders := &failingEmbedder{failAfter: 1}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Embedders:        embedders,
		DedupBehavior:    types.DedupReplace,
	})

	raw := &types.RawDocument{
		SourceURI: "test://replace-fail",
		Data:      []byte("content that will be re-ingested"),
	}

	r1, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Second ingest of identical content: the embedder now fails.
	if _, err := pipe.Ingest(ctx, raw); err == nil {
		t.Fatal("expected ingest error from failing embedder")
	}

	// The prior document must still be retrievable, including by fingerprint.
	doc, err := store.GetDocument(ctx, r1.DocumentUUID)
	if err != nil {
		t.Fatalf("prior document should survive failed replace: %v", err)
	}
	if len(doc.Sections) == 0 || len(doc.Sections[0].Variants) == 0 {
		t.Fatal("prior document lost its sections/variants")
	}
	if _, err := store.FindByFingerprint(ctx, doc.Fingerprint); err != nil {
		t.Errorf("prior document should still be findable by fingerprint: %v", err)
	}
}

// nonReplacerStore hides memstore's ReplaceDocument so the pipeline exercises
// the delete-then-create fallback path.
type nonReplacerStore struct {
	types.Store
}

func TestPipelineReplaceFallbackWithoutReplacer(t *testing.T) {
	ctx := context.Background()
	mem := memstore.New()
	store := &nonReplacerStore{Store: mem}
	embedders := &failingEmbedder{failAfter: 1}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Embedders:        embedders,
		DedupBehavior:    types.DedupReplace,
	})

	raw := &types.RawDocument{
		SourceURI: "test://replace-fallback",
		Data:      []byte("fallback content"),
	}

	r1, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Failure happens before any store mutation, so even without atomic
	// replace the prior document survives.
	if _, err := pipe.Ingest(ctx, raw); err == nil {
		t.Fatal("expected ingest error from failing embedder")
	}
	if _, err := mem.GetDocument(ctx, r1.DocumentUUID); err != nil {
		t.Fatalf("prior document should survive failed replace: %v", err)
	}

	// A successful re-ingest still replaces via the fallback path.
	embedders.failAfter = 100
	r2, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.GetDocument(ctx, r1.DocumentUUID); err != types.ErrDocumentNotFound {
		t.Errorf("old document should be deleted after replace, got err: %v", err)
	}
	if _, err := mem.GetDocument(ctx, r2.DocumentUUID); err != nil {
		t.Errorf("new document should exist: %v", err)
	}
}

// scriptedRetriever returns fixed hits or a fixed error.
type scriptedRetriever struct {
	hits []types.SearchHit
	err  error
}

func (r *scriptedRetriever) Retrieve(_ context.Context, _ string, _ *types.SearchOptions) ([]types.SearchHit, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.hits, nil
}

func textHit(uuid string) types.SearchHit {
	return types.SearchHit{
		Variant: types.ContentVariant{UUID: uuid, ContentType: types.ContentText, Text: uuid},
		Score:   1.0,
	}
}

func TestPipelinePartialSearch(t *testing.T) {
	ctx := context.Background()

	good := &scriptedRetriever{hits: []types.SearchHit{textHit("x")}}
	bad := &scriptedRetriever{err: errors.New("backend down")}

	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{good, bad},
	})

	result, err := pipe.Search(ctx, "query")
	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("expected ErrPartialSearch, got %v", err)
	}
	if result == nil {
		t.Fatal("expected results alongside partial error")
	}
	if len(result.Hits) != 1 || result.Hits[0].Variant.UUID != "x" {
		t.Errorf("expected hit from surviving retriever, got %+v", result.Hits)
	}
}

func TestPipelineSearchAllRetrieversFail(t *testing.T) {
	ctx := context.Background()

	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers: []types.Retriever{
			&scriptedRetriever{err: errors.New("down 1")},
			&scriptedRetriever{err: errors.New("down 2")},
		},
	})

	result, err := pipe.Search(ctx, "query")
	if err == nil {
		t.Fatal("expected error when all retrievers fail")
	}
	if errors.Is(err, types.ErrPartialSearch) {
		t.Error("total failure must not be reported as partial")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

func TestPipelineFusionK(t *testing.T) {
	ctx := context.Background()

	// x is ranked first by one retriever only; y is ranked fourth by both.
	// RRF scores: x = 1/(k+1), y = 2/(k+4). With the default k=60 the
	// cross-retriever consensus wins (y outranks x); with k=1 the single top
	// rank wins (x outranks y). x ties exactly with b's rank-0 hit f3 at any
	// k, so assertions compare x against y, never against f3.
	a := &scriptedRetriever{hits: []types.SearchHit{textHit("x"), textHit("f1"), textHit("f2"), textHit("y")}}
	b := &scriptedRetriever{hits: []types.SearchHit{textHit("f3"), textHit("f4"), textHit("f5"), textHit("y")}}

	rankOf := func(hits []types.SearchHit, uuid string) int {
		for i, h := range hits {
			if h.Variant.UUID == uuid {
				return i
			}
		}
		return -1
	}

	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{a, b},
	})

	def, err := pipe.Search(ctx, "query", types.WithLimit(10))
	if err != nil {
		t.Fatal(err)
	}
	if def.Hits[0].Variant.UUID != "y" {
		t.Errorf("default k=60: expected y first, got %q", def.Hits[0].Variant.UUID)
	}

	lowK, err := pipe.Search(ctx, "query", types.WithLimit(10), types.WithFusionK(1))
	if err != nil {
		t.Fatal(err)
	}
	if xi, yi := rankOf(lowK.Hits, "x"), rankOf(lowK.Hits, "y"); xi < 0 || yi < 0 || xi > yi {
		t.Errorf("k=1: expected x to outrank y, got x at %d, y at %d", xi, yi)
	}

	// Invalid k values fall back to the default.
	invalid, err := pipe.Search(ctx, "query", types.WithLimit(10), types.WithFusionK(-5))
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Hits[0].Variant.UUID != "y" {
		t.Errorf("k<=0 should keep default ranking, got %q first", invalid.Hits[0].Variant.UUID)
	}
}

// mockGraph is a fake knowledge graph that can fail episode ingestion and
// records episode deletions (implementing types.GraphEpisodeDeleter).
type mockGraph struct {
	failIngest    bool
	ingested      []string // GroupIDs of ingested episodes
	deletedGroups []string
}

func (m *mockGraph) ApplyOntology(_ context.Context, _ *knowledgetypes.Ontology) error { return nil }
func (m *mockGraph) IngestEpisode(_ context.Context, in *knowledgetypes.EpisodeInput) (*knowledgetypes.IngestResult, error) {
	if m.failIngest {
		return nil, errors.New("graph unavailable")
	}
	m.ingested = append(m.ingested, in.GroupID)
	return &knowledgetypes.IngestResult{}, nil
}
func (m *mockGraph) GetEntity(_ context.Context, _ string) (*knowledgetypes.Entity, error) {
	return nil, nil
}
func (m *mockGraph) SearchFacts(_ context.Context, _ string, _ ...knowledgetypes.SearchOption) (*knowledgetypes.SearchFactsResult, error) {
	return &knowledgetypes.SearchFactsResult{}, nil
}
func (m *mockGraph) GetGraph(_ context.Context, _ int64) (*knowledgetypes.GraphData, error) {
	return nil, nil
}
func (m *mockGraph) GetNode(_ context.Context, _ string, _ int) (*knowledgetypes.NodeDetail, error) {
	return nil, nil
}
func (m *mockGraph) GetFactProvenance(_ context.Context, _ string) ([]knowledgetypes.Episode, error) {
	return nil, nil
}
func (m *mockGraph) Close(_ context.Context) error { return nil }

func (m *mockGraph) DeleteEpisodes(_ context.Context, groupID string) error {
	m.deletedGroups = append(m.deletedGroups, groupID)
	return nil
}

func TestPipelineIngestGraphFailureIsPartial(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	graph := &mockGraph{failIngest: true}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Graph:            graph,
	})

	result, err := pipe.Ingest(ctx, &types.RawDocument{
		SourceURI: "test://graph-fail",
		Data:      []byte("graph enrichment will fail"),
	})
	if !errors.Is(err, types.ErrPartialIngest) {
		t.Fatalf("expected ErrPartialIngest, got %v", err)
	}
	if result == nil {
		t.Fatal("expected ingest result alongside partial error")
	}

	// The RAG write committed before the graph stage: the doc is retrievable.
	if _, err := store.GetDocument(ctx, result.DocumentUUID); err != nil {
		t.Errorf("document should be committed despite graph failure: %v", err)
	}
}

func TestPipelineDeleteRemovesGraphEpisodes(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	graph := &mockGraph{}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Graph:            graph,
	})

	result, err := pipe.Ingest(ctx, &types.RawDocument{
		SourceURI: "test://graph-delete",
		Data:      []byte("document with graph facts"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := pipe.Delete(ctx, result.DocumentUUID); err != nil {
		t.Fatal(err)
	}

	if len(graph.deletedGroups) != 1 || graph.deletedGroups[0] != result.DocumentUUID {
		t.Errorf("expected DeleteEpisodes(%q), got %v", result.DocumentUUID, graph.deletedGroups)
	}
}

func TestPipelineReplaceRemovesStaleGraphEpisodes(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	graph := &mockGraph{}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Graph:            graph,
		DedupBehavior:    types.DedupReplace,
	})

	raw := &types.RawDocument{
		SourceURI: "test://graph-replace",
		Data:      []byte("content enriched into the graph"),
	}

	r1, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Ingest(ctx, raw); err != nil {
		t.Fatal(err)
	}

	if len(graph.deletedGroups) != 1 || graph.deletedGroups[0] != r1.DocumentUUID {
		t.Errorf("expected stale episodes of %q deleted, got %v", r1.DocumentUUID, graph.deletedGroups)
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
