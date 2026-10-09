package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/parentretriever"
	"github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

// poolRetriever returns opts.Limit hits named h0, h1, ... and records the
// largest limit it was asked for.
type poolRetriever struct {
	total int
	mu    sync.Mutex
	asked int
}

func (r *poolRetriever) Retrieve(_ context.Context, _ string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	r.mu.Lock()
	r.asked = max(r.asked, opts.Limit)
	r.mu.Unlock()
	n := min(opts.Limit, r.total)
	hits := make([]types.SearchHit, n)
	for i := range hits {
		hits[i] = textHit(fmt.Sprintf("h%d", i))
	}
	return hits, nil
}

// favoriteReranker scores one variant highest and leaves the rest in order.
type favoriteReranker struct{ favorite string }

func (r favoriteReranker) Rerank(_ context.Context, _ string, hits []types.SearchHit) ([]types.SearchHit, error) {
	out := slices.Clone(hits)
	slices.SortStableFunc(out, func(a, b types.SearchHit) int {
		switch {
		case a.Variant.UUID == r.favorite:
			return -1
		case b.Variant.UUID == r.favorite:
			return 1
		}
		return 0
	})
	return out, nil
}

func TestPipelineCandidatePool(t *testing.T) {
	tests := []struct {
		name      string
		reranker  types.Reranker
		opts      []types.SearchOption
		wantAsked int
		wantFirst string
	}{
		{
			name:      "reranker gets default pool",
			reranker:  favoriteReranker{favorite: "h15"},
			opts:      []types.SearchOption{types.WithLimit(5)},
			wantAsked: 20,
			wantFirst: "h15",
		},
		{
			name:      "explicit pool",
			reranker:  favoriteReranker{favorite: "h25"},
			opts:      []types.SearchOption{types.WithLimit(5), types.WithCandidatePool(40)},
			wantAsked: 40,
			wantFirst: "h25",
		},
		{
			name:      "pool below limit is raised",
			reranker:  favoriteReranker{favorite: "h3"},
			opts:      []types.SearchOption{types.WithLimit(5), types.WithCandidatePool(2)},
			wantAsked: 5,
			wantFirst: "h3",
		},
		{
			name:      "no reranker keeps limit",
			opts:      []types.SearchOption{types.WithLimit(5)},
			wantAsked: 5,
			wantFirst: "h0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retriever := &poolRetriever{total: 30}
			pipe := pipeline.New(pipeline.Config{
				Store:            memstore.New(),
				ContentExtractor: &simpleExtractor{},
				Retrievers:       []types.Retriever{retriever},
				Reranker:         tt.reranker,
			})
			result, err := pipe.Search(context.Background(), "q", tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if retriever.asked != tt.wantAsked {
				t.Errorf("retriever limit = %d, want %d", retriever.asked, tt.wantAsked)
			}
			if len(result.Hits) != 5 {
				t.Errorf("got %d hits, want 5", len(result.Hits))
			}
			if len(result.Hits) > 0 && result.Hits[0].Variant.UUID != tt.wantFirst {
				t.Errorf("first hit = %s, want %s", result.Hits[0].Variant.UUID, tt.wantFirst)
			}
		})
	}
}

func TestPipelineMinScoreNormalized(t *testing.T) {
	// x is ranked first by both retrievers (normalized 1.0); y is ranked
	// second by one of them (1/62 against a maximum of 2/61, about 0.49).
	a := &scriptedRetriever{hits: []types.SearchHit{textHit("x"), textHit("y")}}
	b := &scriptedRetriever{hits: []types.SearchHit{textHit("x")}}
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{a, b},
	})

	tests := []struct {
		minScore float64
		want     []string
	}{
		{minScore: 0, want: []string{"x", "y"}},
		{minScore: 0.4, want: []string{"x", "y"}},
		{minScore: 0.5, want: []string{"x"}},
		{minScore: 1.0, want: []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.minScore), func(t *testing.T) {
			result, err := pipe.Search(context.Background(), "q", types.WithMinScore(tt.minScore))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range result.Hits {
				got = append(got, h.Variant.UUID)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("hits = %v, want %v", got, tt.want)
			}
		})
	}
}

// tableEmbedder embeds known texts as fixed vectors.
type tableEmbedder map[string][]float32

func (tableEmbedder) Register(types.ContentType, types.VariantEmbedder) {}
func (e tableEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(variants))
	for i, v := range variants {
		vec, ok := e[v.Text]
		if !ok {
			return nil, fmt.Errorf("no vector for %q", v.Text)
		}
		out[i] = vec
	}
	return out, nil
}

func TestPipelineMinScoreWithVectorRetriever(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	embedder := tableEmbedder{
		"query": {1, 0},
		"high":  {0.9, 0.4359}, // cosine 0.9 with the query
		"low":   {0.2, 0.9798}, // cosine 0.2 with the query
	}
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Embedders:        embedder,
		Retrievers:       []types.Retriever{vectorretriever.New(store, embedder)},
	})
	for _, text := range []string{"high", "low"} {
		if _, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte(text)}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := pipe.Search(ctx, "query", types.WithMinScore(0.5))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) == 0 || result.Hits[0].Variant.Text != "high" {
		t.Fatalf("hits = %+v, want the 0.9 hit first", result.Hits)
	}
}

// fixedTransformer returns fixed queries and error.
type fixedTransformer struct {
	queries []string
	err     error
}

func (f fixedTransformer) Transform(context.Context, string) ([]string, error) {
	return f.queries, f.err
}

func TestPipelineQueryTransformFailure(t *testing.T) {
	tests := []struct {
		name        string
		transformer fixedTransformer
		cancel      bool
		wantPartial bool
		wantFatal   bool
		wantQueries int
	}{
		{
			name:        "total failure falls back to raw query",
			transformer: fixedTransformer{err: errors.New("llm down")},
			wantPartial: true,
		},
		{
			name:        "partial expansion keeps produced queries",
			transformer: fixedTransformer{queries: []string{"q", "h1"}, err: errors.New("one generation failed")},
			wantPartial: true,
			wantQueries: 2,
		},
		{
			name:        "canceled context is fatal",
			transformer: fixedTransformer{err: context.Canceled},
			cancel:      true,
			wantFatal:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			pipe := pipeline.New(pipeline.Config{
				Store:            memstore.New(),
				ContentExtractor: &simpleExtractor{},
				QueryTransformer: tt.transformer,
				Retrievers:       []types.Retriever{&scriptedRetriever{hits: []types.SearchHit{textHit("x")}}},
			})
			result, err := pipe.Search(ctx, "q")
			if tt.wantFatal {
				if err == nil || result != nil || errors.Is(err, types.ErrPartialSearch) {
					t.Fatalf("want fatal error and nil result, got %v, %+v", err, result)
				}
				return
			}
			if errors.Is(err, types.ErrPartialSearch) != tt.wantPartial {
				t.Fatalf("err = %v, want partial %v", err, tt.wantPartial)
			}
			if result == nil || len(result.Hits) != 1 {
				t.Fatalf("want one hit, got %+v", result)
			}
			if len(result.TransformedQueries) != tt.wantQueries {
				t.Errorf("transformed queries = %v, want %d", result.TransformedQueries, tt.wantQueries)
			}
		})
	}
}

// sectionCountingStore counts GetSections calls.
type sectionCountingStore struct {
	*memstore.Store
	calls atomic.Int32
}

func (s *sectionCountingStore) GetSections(ctx context.Context, documentUUID string) ([]types.Section, error) {
	s.calls.Add(1)
	return s.Store.GetSections(ctx, documentUUID)
}

func TestPipelineParentExpansionMergesSection(t *testing.T) {
	ctx := context.Background()
	store := &sectionCountingStore{Store: memstore.New()}
	doc := &types.Document{
		UUID: "d1",
		Sections: []types.Section{{
			UUID: "s1", DocumentUUID: "d1",
			Variants: []types.ContentVariant{
				{UUID: "v1", SectionUUID: "s1", ContentType: types.ContentText, Text: "First part."},
				{UUID: "v2", SectionUUID: "s1", ContentType: types.ContentText, Text: "Second part."},
			},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	child := func(uuid string) types.SearchHit {
		return types.SearchHit{
			Variant:    types.ContentVariant{UUID: uuid, ContentType: types.ContentText},
			Score:      1,
			Provenance: types.Provenance{DocumentUUID: "d1", SectionUUID: "s1"},
		}
	}
	// Two arms find different children of the same section.
	a := parentretriever.New(&scriptedRetriever{hits: []types.SearchHit{child("v1")}}, store)
	b := parentretriever.New(&scriptedRetriever{hits: []types.SearchHit{child("v2")}}, store)

	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{a, b},
	})
	result, err := pipe.Search(ctx, "q", types.WithContextAssembly(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("got %d hits, want one per section: %+v", len(result.Hits), result.Hits)
	}
	if result.Hits[0].Provenance.ExpandedFromVariantUUID == "" {
		t.Error("expanded hit must record the variant it was expanded from")
	}
	if want := 2.0 / 61; result.Hits[0].Score != want {
		t.Errorf("score = %v, want summed contributions %v", result.Hits[0].Score, want)
	}
	if n := len(result.Context.Blocks); n != 1 {
		t.Errorf("got %d context blocks, want 1", n)
	}
	if got := store.calls.Load(); got != 2 {
		t.Errorf("GetSections calls = %d, want one per retriever call", got)
	}
}

func TestPipelineFusionMergesDuplicateFields(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	lexical := types.SearchHit{
		Variant: types.ContentVariant{UUID: "x", ContentType: types.ContentText},
		Score:   7.5, // unnormalized lexical score outranks cosine
	}
	vector := types.SearchHit{
		Variant:   types.ContentVariant{UUID: "x", ContentType: types.ContentText, Embedding: []float32{1, 0}},
		Score:     0.8,
		Timestamp: old,
	}
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers: []types.Retriever{
			&scriptedRetriever{hits: []types.SearchHit{lexical}},
			&scriptedRetriever{hits: []types.SearchHit{vector}},
		},
	})
	result, err := pipe.Search(context.Background(), "q", types.WithRecency(24*time.Hour, 1))
	if err != nil {
		t.Fatal(err)
	}
	hit := result.Hits[0]
	if hit.Timestamp.IsZero() || len(hit.Variant.Embedding) == 0 {
		t.Errorf("merged hit lost fields: timestamp %v, embedding %v", hit.Timestamp, hit.Variant.Embedding)
	}
	if undecayed := 2.0 / 61; hit.Score >= undecayed {
		t.Errorf("score %v not decayed below %v", hit.Score, undecayed)
	}
}

// datedExtractor produces a uniquely named document created at a fixed time.
type datedExtractor struct {
	at time.Time
	n  atomic.Int32
}

func (e *datedExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	n := e.n.Add(1)
	doc := fmt.Sprintf("doc-%d", n)
	sec := fmt.Sprintf("sec-%d", n)
	return &types.Document{
		UUID: doc,
		Sections: []types.Section{{
			UUID: sec, DocumentUUID: doc,
			Variants: []types.ContentVariant{{
				UUID: fmt.Sprintf("var-%d", n), SectionUUID: sec,
				ContentType: types.ContentText, Text: string(raw.Data),
			}},
		}},
		CreatedAt: e.at,
	}, nil
}

func TestPipelineRecencyAppliesToLexicalHits(t *testing.T) {
	now := time.Now()
	embedder := tableEmbedder{"zebra": {1, 0}, "zebra stripes": {1, 0.1}}
	tests := []struct {
		name   string
		vector bool
	}{
		{name: "bm25 only"},
		{name: "bm25 and vector", vector: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := memstore.New()
			retrievers := []types.Retriever{bm25retriever.New(store, nil)}
			cfg := pipeline.Config{
				Store:            store,
				ContentExtractor: &datedExtractor{at: now.Add(-60 * 24 * time.Hour)},
			}
			if tt.vector {
				cfg.Embedders = embedder
				retrievers = append(retrievers, vectorretriever.New(store, embedder))
			}
			cfg.Retrievers = retrievers
			pipe := pipeline.New(cfg)
			if _, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("zebra stripes")}); err != nil {
				t.Fatal(err)
			}

			plain, err := pipe.Search(ctx, "zebra")
			if err != nil {
				t.Fatal(err)
			}
			decayed, err := pipe.Search(ctx, "zebra", types.WithRecency(24*time.Hour, 1))
			if err != nil {
				t.Fatal(err)
			}
			if len(plain.Hits) != 1 || len(decayed.Hits) != 1 {
				t.Fatalf("hits = %d and %d, want 1", len(plain.Hits), len(decayed.Hits))
			}
			if decayed.Hits[0].Score >= plain.Hits[0].Score {
				t.Errorf("recency did not decay an old document: %v >= %v", decayed.Hits[0].Score, plain.Hits[0].Score)
			}
		})
	}
}

// partialRetriever returns hits together with a partial error.
type partialRetriever struct{ hits []types.SearchHit }

func (r partialRetriever) Retrieve(context.Context, string, *types.SearchOptions) ([]types.SearchHit, error) {
	return r.hits, fmt.Errorf("%w: section lookup failed", types.ErrPartialSearch)
}

func TestPipelineKeepsPartialRetrieverHits(t *testing.T) {
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{partialRetriever{hits: []types.SearchHit{textHit("x")}}},
	})
	result, err := pipe.Search(context.Background(), "q")
	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("err = %v, want ErrPartialSearch", err)
	}
	if result == nil || len(result.Hits) != 1 {
		t.Fatalf("want the retriever's hit, got %+v", result)
	}
}

func TestPipelineRebuildIndex(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	first := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Retrievers:       []types.Retriever{bm25retriever.New(store, nil)},
	})
	if _, err := first.Ingest(ctx, &types.RawDocument{Data: []byte("the okapi grazes")}); err != nil {
		t.Fatal(err)
	}

	// A new process opens the same store with an empty index.
	second := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Retrievers:       []types.Retriever{parentretriever.New(bm25retriever.New(store, nil), store)},
	})
	before, err := second.Search(ctx, "okapi")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Hits) != 0 {
		t.Fatalf("fresh index returned %d hits", len(before.Hits))
	}
	rebuilder, ok := second.(types.IndexRebuilder)
	if !ok {
		t.Fatal("pipeline must implement types.IndexRebuilder")
	}
	if err := rebuilder.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := second.Search(ctx, "okapi")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Hits) != 1 {
		t.Errorf("rebuilt index returned %d hits, want 1", len(after.Hits))
	}
}
