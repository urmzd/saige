package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/fusion"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

// newScopedPipeline builds a pipeline over a fresh memstore with vector and
// BM25 retrieval.
func newScopedPipeline(scope string, dedup types.DedupBehavior) (types.Pipeline, *memstore.Store) {
	store := memstore.New()
	emb := &simpleEmbedder{}
	return pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: &uniqueExtractor{},
		Embedders:        emb,
		DedupBehavior:    dedup,
		Scope:            scope,
		Retrievers: []types.Retriever{
			vectorretriever.New(store, emb),
			bm25retriever.New(store, nil),
		},
	}), store
}

func TestPipelineScopeIsolation(t *testing.T) {
	data := []byte("shared quarterly report text")
	tests := []struct {
		name  string
		dedup types.DedupBehavior
	}{
		{"dedup skip", types.DedupSkip},
		{"dedup replace", types.DedupReplace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pipe, store := newScopedPipeline("", tt.dedup)

			a, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "a://r", Data: data, Scope: "tenant-a",
				Metadata: map[string]string{"owner": "a"}})
			if err != nil {
				t.Fatal(err)
			}
			b, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "b://r", Data: data, Scope: "tenant-b"})
			if err != nil {
				t.Fatal(err)
			}
			if b.Deduplicated || b.DocumentUUID == a.DocumentUUID {
				t.Fatalf("same bytes in two scopes must make two documents: a=%+v b=%+v", a, b)
			}
			docA, err := store.GetDocument(ctx, a.DocumentUUID)
			if err != nil {
				t.Fatalf("scope A document lost: %v", err)
			}
			if docA.Scope != "tenant-a" || docA.SourceURI != "a://r" {
				t.Errorf("scope A document changed: %+v", docA)
			}

			// A second ingest in B deduplicates (or replaces) within B only.
			b2, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "b://r", Data: data, Scope: "tenant-b"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.dedup == types.DedupSkip && b2.DocumentUUID != b.DocumentUUID {
				t.Errorf("skip in B resolved to %s, want %s", b2.DocumentUUID, b.DocumentUUID)
			}
			if _, err := store.GetDocument(ctx, a.DocumentUUID); err != nil {
				t.Errorf("ingest in B touched A: %v", err)
			}

			for _, scope := range []string{"tenant-a", "tenant-b"} {
				res, err := pipe.Search(ctx, "quarterly report", types.WithScope(scope))
				if err != nil {
					t.Fatal(err)
				}
				if len(res.Hits) == 0 {
					t.Fatalf("scope %s: no hits", scope)
				}
				for _, h := range res.Hits {
					doc, err := store.GetDocument(ctx, h.Provenance.DocumentUUID)
					if err != nil || doc.Scope != scope {
						t.Errorf("scope %s search returned document of scope %q", scope, doc.Scope)
					}
				}
			}
			res, err := pipe.Search(ctx, "quarterly report")
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Hits) != 0 {
				t.Errorf("default-scope search returned %d scoped hits", len(res.Hits))
			}
		})
	}
}

func TestPipelineFixedScope(t *testing.T) {
	ctx := context.Background()
	other, store := newScopedPipeline("", types.DedupSkip)
	foreign, err := other.Ingest(ctx, &types.RawDocument{SourceURI: "x://1", Data: []byte("foreign tenant notes"), Scope: "other"})
	if err != nil {
		t.Fatal(err)
	}

	emb := &simpleEmbedder{}
	ext := &uniqueExtractor{}
	ext.counter.Store(100) // distinct UUIDs from the other pipeline's documents
	pipe := pipeline.New(pipeline.Config{
		Store:            store,
		ContentExtractor: ext,
		Embedders:        emb,
		Scope:            "mine",
		Retrievers:       []types.Retriever{vectorretriever.New(store, emb)},
	})
	own, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "m://1", Data: []byte("my tenant notes")})
	if err != nil {
		t.Fatal(err)
	}
	if doc, _ := store.GetDocument(ctx, own.DocumentUUID); doc.Scope != "mine" {
		t.Errorf("ingest without scope stored scope %q, want the pipeline's", doc.Scope)
	}

	tests := []struct {
		name    string
		call    func() error
		wantErr error
	}{
		{"ingest naming another scope", func() error {
			_, err := pipe.Ingest(ctx, &types.RawDocument{Data: []byte("x"), Scope: "other"})
			return err
		}, types.ErrScopeMismatch},
		{"search naming another scope", func() error {
			_, err := pipe.Search(ctx, "notes", types.WithScope("other"))
			return err
		}, types.ErrScopeMismatch},
		{"lookup of a foreign variant", func() error {
			doc, _ := store.GetDocument(ctx, foreign.DocumentUUID)
			_, err := pipe.Lookup(ctx, doc.Sections[0].Variants[0].UUID)
			return err
		}, types.ErrVariantNotFound},
		{"reconstruct of a foreign document", func() error {
			_, err := pipe.Reconstruct(ctx, foreign.DocumentUUID)
			return err
		}, types.ErrDocumentNotFound},
		{"lookup of an own variant", func() error {
			doc, _ := store.GetDocument(ctx, own.DocumentUUID)
			_, err := pipe.Lookup(ctx, doc.Sections[0].Variants[0].UUID)
			return err
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}

	if err := pipe.Delete(ctx, foreign.DocumentUUID); err != nil {
		t.Fatalf("delete of a foreign document should be a no-op: %v", err)
	}
	if _, err := store.GetDocument(ctx, foreign.DocumentUUID); err != nil {
		t.Errorf("delete through a fixed scope removed a foreign document: %v", err)
	}
	if _, err := pipe.Update(ctx, foreign.DocumentUUID, &types.RawDocument{Data: []byte("overwrite")}); err != nil {
		t.Fatal(err)
	}
	if doc, _ := store.GetDocument(ctx, foreign.DocumentUUID); doc.Sections[0].Variants[0].Text != "foreign tenant notes" {
		t.Errorf("update through a fixed scope changed a foreign document")
	}
}

func TestPipelineUpdateScope(t *testing.T) {
	ctx := context.Background()
	pipe, store := newScopedPipeline("", types.DedupSkip)
	res, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "a://1", Data: []byte("v1"), Scope: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Update(ctx, res.DocumentUUID, &types.RawDocument{Data: []byte("v2"), Scope: "b"}); !errors.Is(err, types.ErrScopeMismatch) {
		t.Fatalf("moving a document to another scope: err = %v, want ErrScopeMismatch", err)
	}
	if _, err := pipe.Update(ctx, res.DocumentUUID, &types.RawDocument{SourceURI: "a://1", Data: []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	doc, err := store.GetDocument(ctx, res.DocumentUUID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Scope != "a" || doc.Fingerprint != types.Fingerprint("a", []byte("v2")) {
		t.Errorf("update without a scope must keep the document's: scope=%q fingerprint=%s", doc.Scope, doc.Fingerprint)
	}
}

// datedRaw builds a raw document with a source modification time.
func datedRaw(uri, text string, modified time.Time) *types.RawDocument {
	return &types.RawDocument{SourceURI: uri, Data: []byte(text), SourceModifiedAt: modified}
}

func TestPipelineTimeRangeAndRecency(t *testing.T) {
	ctx := context.Background()
	pipe, store := newScopedPipeline("", types.DedupSkip)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	old, err := pipe.Ingest(ctx, datedRaw("f://old", "release notes alpha", now.AddDate(-1, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	recent, err := pipe.Ingest(ctx, datedRaw("f://new", "release notes beta", now.AddDate(0, 0, -1)))
	if err != nil {
		t.Fatal(err)
	}
	if doc, _ := store.GetDocument(ctx, old.DocumentUUID); !doc.SourceModifiedAt.Equal(now.AddDate(-1, 0, 0)) {
		t.Fatalf("source modified time not stored: %v", doc.SourceModifiedAt)
	}

	tests := []struct {
		name  string
		since time.Time
		until time.Time
		want  []string
	}{
		{"no bounds", time.Time{}, time.Time{}, []string{old.DocumentUUID, recent.DocumentUUID}},
		{"since keeps recent", now.AddDate(0, -1, 0), time.Time{}, []string{recent.DocumentUUID}},
		{"until keeps old", time.Time{}, now.AddDate(0, -1, 0), []string{old.DocumentUUID}},
		{"empty window", now.AddDate(0, -6, 0), now.AddDate(0, -5, 0), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := pipe.Search(ctx, "release notes", types.WithTimeRange(tt.since, tt.until))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, h := range res.Hits {
				got[h.Provenance.DocumentUUID] = true
			}
			if len(got) != len(tt.want) {
				t.Fatalf("documents = %v, want %v", got, tt.want)
			}
			for _, w := range tt.want {
				if !got[w] {
					t.Errorf("missing document %s", w)
				}
			}
		})
	}

	// Recency decays by the source time, not the ingest time: both were
	// ingested just now, but the year-old one must rank lower.
	res, err := pipe.Search(ctx, "release notes",
		types.WithRecency(30*24*time.Hour, 1),
		func(c *types.SearchConfig) { c.RecencyNow = now })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Provenance.DocumentUUID != recent.DocumentUUID {
		t.Fatalf("recency must prefer the recently modified source: %+v", res.Hits)
	}
}

// fixedListRetriever returns the same hits for every query.
type fixedListRetriever struct {
	name string
	hits []types.SearchHit
	err  error
}

func (r fixedListRetriever) Name() string { return r.name }
func (r fixedListRetriever) Retrieve(context.Context, string, *types.SearchOptions) ([]types.SearchHit, error) {
	return r.hits, r.err
}

func hitWithText(id, text string) types.SearchHit {
	return types.SearchHit{
		Variant:    types.ContentVariant{UUID: id, ContentType: types.ContentText, Text: text},
		Provenance: types.Provenance{DocumentUUID: "d-" + id, SectionUUID: "s-" + id},
	}
}

func TestPipelineContentDedup(t *testing.T) {
	ctx := context.Background()
	r := fixedListRetriever{name: "fixed", hits: []types.SearchHit{
		hitWithText("a", "Copyright notice."),
		hitWithText("b", "  Copyright notice.\n"),
		hitWithText("c", "Actual answer."),
	}}
	pipe := pipeline.New(pipeline.Config{Store: memstore.New(), ContentExtractor: &simpleExtractor{}, Retrievers: []types.Retriever{r}})

	tests := []struct {
		name string
		opts []types.SearchOption
		want []string
	}{
		{"off by default", nil, []string{"a", "b", "c"}},
		{"collapses identical trimmed text", []types.SearchOption{types.WithContentDedup()}, []string{"a", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := pipe.Search(ctx, "q", tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range res.Hits {
				got = append(got, h.Variant.UUID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hits = %v, want %v", got, tt.want)
			}
		})
	}
}

// sectionsExtractor makes one section per "|"-separated part of the data.
type sectionsExtractor struct{}

func (sectionsExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	doc := &types.Document{UUID: "doc-" + raw.SourceURI, SourceURI: raw.SourceURI}
	for i, part := range strings.Split(string(raw.Data), "|") {
		secUUID := fmt.Sprintf("%s-s%d", raw.SourceURI, i)
		doc.Sections = append(doc.Sections, types.Section{
			UUID: secUUID, DocumentUUID: doc.UUID, Index: i,
			Variants: []types.ContentVariant{{
				UUID: secUUID + "-v", SectionUUID: secUUID, ContentType: types.ContentText, Text: part,
			}},
		})
	}
	return doc, nil
}

func TestPipelineNeighborWindow(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	pipe := pipeline.New(pipeline.Config{Store: store, ContentExtractor: sectionsExtractor{}, Retrievers: []types.Retriever{fixedListRetriever{}}})
	if _, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "w", Data: []byte("zero|one|two|three|four|five")}); err != nil {
		t.Fatal(err)
	}
	hit := func(i int) types.SearchHit {
		id := fmt.Sprintf("w-s%d", i)
		return types.SearchHit{
			Variant:    types.ContentVariant{UUID: id + "-v", ContentType: types.ContentText, Text: []string{"zero", "one", "two", "three", "four", "five"}[i]},
			Provenance: types.Provenance{DocumentUUID: "doc-w", SectionUUID: id, SectionIndex: i},
		}
	}

	tests := []struct {
		name       string
		hits       []types.SearchHit
		window     int
		wantTexts  []string
		wantRanges [][2]int
	}{
		{
			name: "window of one", hits: []types.SearchHit{hit(2)}, window: 1,
			wantTexts: []string{"one\n\ntwo\n\nthree"}, wantRanges: [][2]int{{1, 3}},
		},
		{
			name: "clipped at the document start", hits: []types.SearchHit{hit(0)}, window: 2,
			wantTexts: []string{"zero\n\none\n\ntwo"}, wantRanges: [][2]int{{0, 2}},
		},
		{
			name: "covered hit dropped, later window stops at covered sections", hits: []types.SearchHit{hit(1), hit(2), hit(4)}, window: 1,
			wantTexts:  []string{"zero\n\none\n\ntwo", "three\n\nfour\n\nfive"},
			wantRanges: [][2]int{{0, 2}, {3, 5}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := pipeline.New(pipeline.Config{Store: store, ContentExtractor: sectionsExtractor{},
				Retrievers: []types.Retriever{fixedListRetriever{name: "fixed", hits: tt.hits}}})
			res, err := p.Search(ctx, "q", types.WithNeighborWindow(tt.window), types.WithFusionK(1))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Hits) != len(tt.wantTexts) {
				t.Fatalf("got %d hits, want %d: %+v", len(res.Hits), len(tt.wantTexts), res.Hits)
			}
			for i, h := range res.Hits {
				if h.Variant.Text != tt.wantTexts[i] {
					t.Errorf("hit %d text = %q, want %q", i, h.Variant.Text, tt.wantTexts[i])
				}
				w := h.Provenance.Window
				if w == nil || w.First != tt.wantRanges[i][0] || w.Last != tt.wantRanges[i][1] {
					t.Errorf("hit %d window = %+v, want %v", i, w, tt.wantRanges[i])
				}
			}
		})
	}
}

func TestPipelineRetrievalStatsAndFuser(t *testing.T) {
	ctx := context.Background()
	lexical := fixedListRetriever{name: "lexical", hits: []types.SearchHit{hitWithText("x", "x"), hitWithText("y", "y")}}
	dense := fixedListRetriever{name: "dense", hits: []types.SearchHit{hitWithText("y", "y"), hitWithText("x", "x")}}
	broken := fixedListRetriever{name: "broken", err: errors.New("down")}

	tests := []struct {
		name    string
		fuser   types.Fuser
		wantTop string
	}{
		{"rrf ties broken by id", nil, "x"},
		{"weighted trusts dense", fusion.Weighted{Weights: map[string]float64{"dense": 3}}, "y"},
		{"weighted trusts lexical", fusion.Weighted{Weights: map[string]float64{"lexical": 3}}, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipe := pipeline.New(pipeline.Config{
				Store: memstore.New(), ContentExtractor: &simpleExtractor{},
				Retrievers: []types.Retriever{lexical, dense, broken},
				Fuser:      tt.fuser,
			})
			res, err := pipe.Search(ctx, "q")
			if !errors.Is(err, types.ErrPartialSearch) {
				t.Fatalf("err = %v, want ErrPartialSearch", err)
			}
			if res.Hits[0].Variant.UUID != tt.wantTop {
				t.Errorf("top = %s, want %s", res.Hits[0].Variant.UUID, tt.wantTop)
			}
			if len(res.Retrievals) != 3 {
				t.Fatalf("retrievals = %+v", res.Retrievals)
			}
			want := []types.RetrievalStat{
				{Retriever: "lexical", Hits: 2},
				{Retriever: "dense", Hits: 2},
				{Retriever: "broken", Hits: 0, Error: "down"},
			}
			for i, w := range want {
				got := res.Retrievals[i]
				if got.Retriever != w.Retriever || got.Hits != w.Hits || got.Error != w.Error {
					t.Errorf("retrieval %d = %+v, want %+v", i, got, w)
				}
			}
		})
	}
}

// recordingObserver records spans with their parent span name.
type recordingObserver struct {
	types.NoopObserver
	mu         sync.Mutex
	spans      []*recordedSpan
	embeddings []types.EmbeddingRecord
	retrievals []types.RetrievalRecord
}

type recordedSpan struct {
	name   string
	parent string
	attrs  map[string]any
	err    error
	ended  bool
	mu     *sync.Mutex
}

type spanKey struct{}

func (o *recordingObserver) StartSpan(ctx context.Context, name string, attrs ...types.Attribute) (context.Context, types.Span) {
	parent := ""
	if p, ok := ctx.Value(spanKey{}).(*recordedSpan); ok {
		parent = p.name
	}
	s := &recordedSpan{name: name, parent: parent, attrs: map[string]any{}, mu: &o.mu}
	s.SetAttributes(attrs...)
	o.mu.Lock()
	o.spans = append(o.spans, s)
	o.mu.Unlock()
	return context.WithValue(ctx, spanKey{}, s), s
}

func (o *recordingObserver) RecordEmbedding(_ context.Context, rec types.EmbeddingRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.embeddings = append(o.embeddings, rec)
}

func (o *recordingObserver) RecordRetrieval(_ context.Context, rec types.RetrievalRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retrievals = append(o.retrievals, rec)
}

func (s *recordedSpan) SetAttributes(attrs ...types.Attribute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range attrs {
		s.attrs[a.Key] = a.Value
	}
}
func (s *recordedSpan) RecordError(err error) { s.err = err }
func (s *recordedSpan) End()                  { s.ended = true }

func (o *recordingObserver) children(parent string) map[string][]*recordedSpan {
	out := map[string][]*recordedSpan{}
	for _, s := range o.spans {
		if s.parent == parent {
			out[s.name] = append(out[s.name], s)
		}
	}
	return out
}

func TestPipelineObserver(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	emb := &simpleEmbedder{}
	obs := &recordingObserver{}
	pipe := pipeline.New(pipeline.Config{
		Store: store, ContentExtractor: &uniqueExtractor{}, Embedders: emb,
		Retrievers: []types.Retriever{vectorretriever.New(store, emb), bm25retriever.New(store, nil)},
		Reranker:   favoriteReranker{},
		Observer:   obs,
	})
	if _, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: "o://1", Data: []byte("observable pipeline text")}); err != nil {
		t.Fatal(err)
	}

	ingest := obs.children("")[types.SpanIngest]
	if len(ingest) != 1 || !ingest[0].ended {
		t.Fatalf("ingest spans = %+v", ingest)
	}
	stages := obs.children(types.SpanIngest)
	for _, name := range []string{types.SpanExtract, types.SpanEmbed, types.SpanWrite, types.SpanIndex} {
		if len(stages[name]) == 0 {
			t.Errorf("ingest has no %s child", name)
		}
	}
	if len(obs.embeddings) != 1 || obs.embeddings[0].Inputs != 1 || obs.embeddings[0].Purpose != types.PurposeDocument {
		t.Errorf("embedding records = %+v", obs.embeddings)
	}

	obs.spans = nil
	if _, err := pipe.Search(ctx, "observable text"); err != nil {
		t.Fatal(err)
	}
	search := obs.children("")[types.SpanSearch]
	if len(search) != 1 {
		t.Fatalf("search spans = %d", len(search))
	}
	children := obs.children(types.SpanSearch)
	retrieves := children[types.SpanRetrieve]
	if len(retrieves) != 2 || len(children[types.SpanRerank]) != 1 || len(children[types.SpanFuse]) != 1 {
		t.Fatalf("search children = %v", children)
	}
	names := map[any]bool{}
	for _, s := range retrieves {
		names[s.attrs[types.AttrRetriever]] = true
		if _, ok := s.attrs[types.AttrHits]; !ok {
			t.Errorf("retrieve span %v has no hit count", s.attrs)
		}
	}
	if !names["vector"] || !names["bm25"] {
		t.Errorf("retrieve spans named %v", names)
	}
	if len(obs.retrievals) != 2 {
		t.Errorf("retrieval records = %+v", obs.retrievals)
	}
}
