package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/embeddingcache"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

// setIndexer records which documents are currently indexed.
type setIndexer struct {
	mu   sync.Mutex
	docs map[string]bool
	fail atomic.Int32 // number of Index calls that fail before succeeding
}

func newSetIndexer() *setIndexer { return &setIndexer{docs: make(map[string]bool)} }

func (s *setIndexer) Retrieve(context.Context, string, *types.SearchOptions) ([]types.SearchHit, error) {
	return nil, nil
}

func (s *setIndexer) Index(_ context.Context, doc *types.Document) error {
	if s.fail.Load() > 0 {
		s.fail.Add(-1)
		return errors.New("index unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[doc.UUID] = true
	return nil
}

func (s *setIndexer) Remove(_ context.Context, documentUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, documentUUID)
	return nil
}

func (s *setIndexer) indexed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for d := range s.docs {
		out = append(out, d)
	}
	return out
}

func TestPipelineDedupReplaceRemovesStaleIndexEntries(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	idx := newSetIndexer()
	bm25 := bm25retriever.New(store, nil)

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		DedupBehavior:    types.DedupReplace,
		Retrievers:       []types.Retriever{idx, bm25},
	})
	raw := &types.RawDocument{SourceURI: "test://replace", Data: []byte("the zebra runs fast")}

	if _, err := pipe.Ingest(ctx, raw); err != nil {
		t.Fatal(err)
	}
	r2, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	if got := idx.indexed(); len(got) != 1 || got[0] != r2.DocumentUUID {
		t.Errorf("indexed documents = %v, want only %s", got, r2.DocumentUUID)
	}
	hits, err := bm25.Retrieve(ctx, "zebra", &types.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Provenance.DocumentUUID != r2.DocumentUUID {
		t.Errorf("bm25 hits = %+v, want one hit from %s", hits, r2.DocumentUUID)
	}
}

func TestPipelineUpdateFailureKeepsDocument(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	bm25 := bm25retriever.New(store, nil)
	embedders := &failingEmbedder{failAfter: 1}

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Embedders:        embedders,
		Retrievers:       []types.Retriever{bm25},
	})
	r1, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "test://u", Data: []byte("the zebra runs fast")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = pipe.Update(ctx, r1.DocumentUUID, &types.RawDocument{SourceURI: "test://u", Data: []byte("the lion sleeps")})
	if err == nil {
		t.Fatal("expected update to fail with the embedder error")
	}

	doc, err := store.GetDocument(ctx, r1.DocumentUUID)
	if err != nil {
		t.Fatalf("document lost after failed update: %v", err)
	}
	if doc.Sections[0].Variants[0].Text != "the zebra runs fast" {
		t.Errorf("document content changed: %q", doc.Sections[0].Variants[0].Text)
	}
	hits, err := bm25.Retrieve(ctx, "zebra", &types.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("bm25 postings lost after failed update: %d hits", len(hits))
	}
}

func TestPipelineUpdateReplacesIndexEntries(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	bm25 := bm25retriever.New(store, nil)
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Retrievers:       []types.Retriever{bm25},
	})
	r1, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("the zebra runs fast")})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := pipe.Update(ctx, r1.DocumentUUID, &types.RawDocument{Data: []byte("the lion sleeps")})
	if err != nil {
		t.Fatal(err)
	}
	if r2.DocumentUUID != r1.DocumentUUID {
		t.Errorf("update changed UUID to %s", r2.DocumentUUID)
	}
	for query, want := range map[string]int{"zebra": 0, "lion": 1} {
		hits, err := bm25.Retrieve(ctx, query, &types.SearchOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != want {
			t.Errorf("query %q: got %d hits, want %d", query, len(hits), want)
		}
	}
}

func TestPipelineUpdateDedup(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		dedup     types.DedupBehavior
		content   func(otherData string) string
		wantErr   error
		wantDedup bool
	}{
		{name: "content of another document", dedup: types.DedupSkip, content: func(o string) string { return o }, wantErr: types.ErrDuplicateDocument},
		{name: "content of another document with replace", dedup: types.DedupReplace, content: func(o string) string { return o }, wantErr: types.ErrDuplicateDocument},
		{name: "unchanged content skips", dedup: types.DedupSkip, content: func(string) string { return "target content" }, wantDedup: true},
		{name: "unchanged content replaces", dedup: types.DedupReplace, content: func(string) string { return "target content" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := memstore.New()
			pipe := pipeline.New(pipeline.Config{
				Store:            store,
				ContentExtractor: &uniqueExtractor{},
				DedupBehavior:    tt.dedup,
			})
			target, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("target content")})
			if err != nil {
				t.Fatal(err)
			}
			other, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("other content")})
			if err != nil {
				t.Fatal(err)
			}

			result, err := pipe.Update(ctx, target.DocumentUUID, &types.RawDocument{Data: []byte(tt.content("other content"))})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				for _, id := range []string{target.DocumentUUID, other.DocumentUUID} {
					if _, err := store.GetDocument(ctx, id); err != nil {
						t.Errorf("document %s lost: %v", id, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.DocumentUUID != target.DocumentUUID || result.Deduplicated != tt.wantDedup {
				t.Errorf("result = %+v, want UUID %s deduplicated=%v", result, target.DocumentUUID, tt.wantDedup)
			}
		})
	}
}

func TestPipelineUpdateMissingDocumentIngests(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	pipe := pipeline.New(pipeline.Config{Store: store, ContentExtractor: &uniqueExtractor{}})
	result, err := pipe.Update(ctx, "missing", &types.RawDocument{Data: []byte("fresh")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDocument(ctx, result.DocumentUUID); err != nil {
		t.Errorf("ingested document not found: %v", err)
	}
}

// shapeRegistry returns a malformed embedding result.
type shapeRegistry struct {
	out func(n int) [][]float32
}

func (shapeRegistry) Register(types.ContentType, types.VariantEmbedder) {}
func (s shapeRegistry) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	return s.out(len(variants)), nil
}

func TestPipelineRejectsMalformedEmbeddings(t *testing.T) {
	tests := []struct {
		name string
		out  func(n int) [][]float32
	}{
		{name: "one vector short", out: func(n int) [][]float32 { return make([][]float32, n-1) }},
		{name: "nil vector", out: func(n int) [][]float32 { return make([][]float32, n) }},
		{name: "empty vector", out: func(n int) [][]float32 {
			out := make([][]float32, n)
			for i := range out {
				out[i] = []float32{}
			}
			return out
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := memstore.New()
			pipe := pipeline.New(pipeline.Config{
				Store:            store,
				ContentExtractor: &simpleExtractor{},
				Embedders:        shapeRegistry{out: tt.out},
			})
			_, err := pipe.Ingest(context.Background(), &types.RawDocument{Data: []byte("text")})
			if !errors.Is(err, types.ErrEmbeddingShape) {
				t.Fatalf("expected ErrEmbeddingShape, got %v", err)
			}
			if _, err := store.GetDocument(context.Background(), "test-doc"); !errors.Is(err, types.ErrDocumentNotFound) {
				t.Errorf("document stored despite malformed embeddings: %v", err)
			}
		})
	}
}

// dirtyExtractor emits text that PostgreSQL TEXT and JSONB columns reject.
type dirtyExtractor struct{}

func (dirtyExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	return &types.Document{
		UUID:     "dirty",
		Title:    "t\x00",
		Metadata: map[string]string{"k": "v\x00"},
		Sections: []types.Section{{
			UUID: "s", DocumentUUID: "dirty", Heading: "h\xff",
			Variants: []types.ContentVariant{{
				UUID: "v", SectionUUID: "s", ContentType: types.ContentText,
				Text: string(raw.Data), Metadata: map[string]string{"m": "\x00x"},
			}},
		}},
	}, nil
}

func TestPipelineCleansExtractedText(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	pipe := pipeline.New(pipeline.Config{Store: store, ContentExtractor: dirtyExtractor{}})
	if _, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("a\x00b\xffc")}); err != nil {
		t.Fatal(err)
	}
	doc, err := store.GetDocument(ctx, "dirty")
	if err != nil {
		t.Fatal(err)
	}
	v := doc.Sections[0].Variants[0]
	for field, got := range map[string]string{
		"title": doc.Title, "doc metadata": doc.Metadata["k"], "heading": doc.Sections[0].Heading,
		"text": v.Text, "variant metadata": v.Metadata["m"],
	} {
		if !utf8.ValidString(got) || strings.IndexByte(got, 0) >= 0 {
			t.Errorf("%s not cleaned: %q", field, got)
		}
	}
	if v.Text != "ab�c" {
		t.Errorf("text = %q, want %q", v.Text, "ab�c")
	}
}

// purposeRegistry records the embed purpose of every call.
type purposeRegistry struct {
	mu       sync.Mutex
	purposes []types.EmbedPurpose
}

func (p *purposeRegistry) Register(types.ContentType, types.VariantEmbedder) {}
func (p *purposeRegistry) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	p.mu.Lock()
	p.purposes = append(p.purposes, types.EmbedPurposeFrom(ctx))
	p.mu.Unlock()
	out := make([][]float32, len(variants))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestPipelineEmbedPurpose(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	reg := &purposeRegistry{}
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Embedders:        reg,
		Retrievers:       []types.Retriever{vectorretriever.New(store, reg)},
	})
	if _, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("text")}); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Search(ctx, "query"); err != nil {
		t.Fatal(err)
	}
	want := []types.EmbedPurpose{types.PurposeDocument, types.PurposeQuery}
	if len(reg.purposes) != 2 || reg.purposes[0] != want[0] || reg.purposes[1] != want[1] {
		t.Errorf("purposes = %v, want %v", reg.purposes, want)
	}
}

// countingStore counts successful document writes.
type countingStore struct {
	*memstore.Store
	creates atomic.Int32
}

func (c *countingStore) CreateDocument(ctx context.Context, doc *types.Document) error {
	err := c.Store.CreateDocument(ctx, doc)
	if err == nil {
		c.creates.Add(1)
	}
	return err
}

func TestPipelineConcurrentDuplicateIngest(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Store: memstore.New()}
	idx := newSetIndexer()
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Retrievers:       []types.Retriever{idx},
	})
	raw := &types.RawDocument{SourceURI: "test://race", Data: []byte("same content")}

	const workers = 16
	results := make([]*types.IngestResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = pipe.Ingest(ctx, raw)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if results[i].DocumentUUID != results[0].DocumentUUID {
			t.Errorf("worker %d got %s, worker 0 got %s", i, results[i].DocumentUUID, results[0].DocumentUUID)
		}
	}
	if n := store.creates.Load(); n != 1 {
		t.Errorf("stored %d documents, want 1", n)
	}
	if got := idx.indexed(); len(got) != 1 {
		t.Errorf("indexed %v, want exactly one document", got)
	}
}

// originalFailStore fails StoreOriginal.
type originalFailStore struct {
	*memstore.Store
}

func (originalFailStore) StoreOriginal(context.Context, string, []byte) error {
	return errors.New("blob store unavailable")
}

func TestPipelinePostCommitFailuresArePartial(t *testing.T) {
	tests := []struct {
		name  string
		setup func() (types.Store, []types.Retriever, bool)
	}{
		{
			name: "indexer fails once",
			setup: func() (types.Store, []types.Retriever, bool) {
				idx := newSetIndexer()
				idx.fail.Store(1)
				return memstore.New(), []types.Retriever{idx}, false
			},
		},
		{
			name: "store original fails",
			setup: func() (types.Store, []types.Retriever, bool) {
				return originalFailStore{Store: memstore.New()}, nil, true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, retrievers, storeOriginals := tt.setup()
			pipe := pipeline.New(pipeline.Config{
				Store:            store,
				ContentExtractor: &simpleExtractor{},
				Retrievers:       retrievers,
				StoreOriginals:   storeOriginals,
			})
			result, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("committed")})
			if !errors.Is(err, types.ErrPartialIngest) {
				t.Fatalf("expected ErrPartialIngest, got %v", err)
			}
			if result == nil || result.DocumentUUID != "test-doc" {
				t.Fatalf("expected a result for the committed document, got %+v", result)
			}
			if _, err := store.GetDocument(ctx, "test-doc"); err != nil {
				t.Errorf("document should be committed: %v", err)
			}
		})
	}
}

// innerCounter counts inputs that reach the provider-side embedder.
type innerCounter struct {
	inputs atomic.Int32
}

func (c *innerCounter) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	c.inputs.Add(int32(len(variants)))
	out := make([][]float32, len(variants))
	for i, v := range variants {
		out[i] = []float32{float32(len(v.Text)), 1}
	}
	return out, nil
}

func TestPipelineReplaceReusesCachedEmbeddings(t *testing.T) {
	ctx := context.Background()
	inner := &innerCounter{}
	reg := embedderregistry.NewTextOnly(embeddingcache.New(inner))
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &uniqueExtractor{},
		Embedders:        reg,
		DedupBehavior:    types.DedupReplace,
	})
	raw := &types.RawDocument{Data: []byte("unchanged paragraph")}
	if _, err := pipe.Ingest(ctx, raw); err != nil {
		t.Fatal(err)
	}
	first := inner.inputs.Load()
	if _, err := pipe.Ingest(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if extra := inner.inputs.Load() - first; extra != 0 {
		t.Errorf("re-ingest embedded %d inputs again, want 0", extra)
	}
}

// deleteFailStore fails every DeleteDocument call while fail is set.
type deleteFailStore struct {
	types.Store
	fail bool
}

func (s *deleteFailStore) DeleteDocument(ctx context.Context, documentUUID string) error {
	if s.fail {
		return errors.New("store unavailable")
	}
	return s.Store.DeleteDocument(ctx, documentUUID)
}

func TestPipelineDeleteKeepsIndexWhenStoreFails(t *testing.T) {
	tests := []struct {
		name       string
		storeFails bool
		wantErr    bool
		wantHits   int
		wantStored bool
	}{
		{name: "store delete fails", storeFails: true, wantErr: true, wantHits: 1, wantStored: true},
		{name: "store delete succeeds", storeFails: false, wantErr: false, wantHits: 0, wantStored: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mem := memstore.New()
			store := &deleteFailStore{Store: mem}
			bm25 := bm25retriever.New(store, nil)
			pipe := pipeline.New(pipeline.Config{
				Store:            store,
				ContentExtractor: &uniqueExtractor{},
				Retrievers:       []types.Retriever{bm25},
			})
			r, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "test://d", Data: []byte("the zebra runs fast")})
			if err != nil {
				t.Fatal(err)
			}

			store.fail = tt.storeFails
			err = pipe.Delete(ctx, r.DocumentUUID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Delete error = %v, wantErr %v", err, tt.wantErr)
			}
			_, getErr := mem.GetDocument(ctx, r.DocumentUUID)
			if stored := getErr == nil; stored != tt.wantStored {
				t.Errorf("document stored = %v, want %v", stored, tt.wantStored)
			}
			hits, err := bm25.Retrieve(ctx, "zebra", &types.SearchOptions{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != tt.wantHits {
				t.Errorf("bm25 hits = %d, want %d", len(hits), tt.wantHits)
			}
		})
	}
}
