package rag_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/reranker"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// constEmbedder embeds every variant as the same vector.
type constEmbedder struct{}

func (constEmbedder) Register(ragtypes.ContentType, ragtypes.VariantEmbedder) {}

func (constEmbedder) Embed(_ context.Context, vs []ragtypes.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(vs))
	for i := range vs {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func retrieverNames(stats []ragtypes.RetrievalStat) []string {
	var names []string
	for _, s := range stats {
		names = append(names, s.Retriever)
	}
	slices.Sort(names)
	return names
}

// A pipeline with embedders over a store with a keyword index fuses keyword
// search by default, unless the retrievers are explicit or opted out.
func TestDefaultHybridKeywordSearch(t *testing.T) {
	tests := []struct {
		name string
		opts []rag.Option
		want []string
	}{
		{"automatic retrievers", nil, []string{"bm25", "vector"}},
		{"opted out", []rag.Option{rag.WithoutKeywordSearch()}, []string{"vector"}},
		{"explicit retrievers", []rag.Option{rag.WithRetrievers(rankedRetriever{"custom", []string{"a"}})}, []string{"custom"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &keywordStore{Store: memstore.New()}
			pipe, err := rag.New(rag.Config{}, append([]rag.Option{
				rag.WithStore(store),
				rag.WithContentExtractor(&stubExtractor{}),
				rag.WithEmbedders(constEmbedder{}),
			}, tt.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipe.Search(context.Background(), "ERR-4012")
			if err != nil {
				t.Fatal(err)
			}
			if got := retrieverNames(result.Retrievals); !slices.Equal(got, tt.want) {
				t.Errorf("retrievers = %v, want %v", got, tt.want)
			}
		})
	}
}

// A pipeline with embedders chunks long documents by default, so they are
// not embedded whole and cut off at the embedder's input limit.
func TestDefaultChunkerWithEmbedders(t *testing.T) {
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 400)
	tests := []struct {
		name     string
		opts     []rag.Option
		chunked  bool
		sections int
	}{
		{"default", nil, true, 0},
		{"opted out", []rag.Option{rag.WithoutChunker()}, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipe, err := rag.New(rag.Config{}, append([]rag.Option{
				rag.WithStore(memstore.New()),
				rag.WithContentExtractor(&stubExtractor{}),
				rag.WithEmbedders(constEmbedder{}),
			}, tt.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			res, err := pipe.Ingest(context.Background(), &ragtypes.RawDocument{SourceURI: "mem://long", Data: []byte(long)})
			if err != nil {
				t.Fatal(err)
			}
			if tt.chunked && res.Sections < 2 {
				t.Errorf("sections = %d, want the long text chunked", res.Sections)
			}
			if !tt.chunked && res.Sections != tt.sections {
				t.Errorf("sections = %d, want %d", res.Sections, tt.sections)
			}
		})
	}
}

// fixedScorer scores each document by a table.
type fixedScorer map[string]float64

func (s fixedScorer) Score(_ context.Context, pairs []reranker.QueryDocPair) ([]float64, error) {
	out := make([]float64, len(pairs))
	for i, p := range pairs {
		out[i] = s[p.Document]
	}
	return out, nil
}

// embeddedRetriever returns hits whose text is their UUID, with embeddings.
type embeddedRetriever map[string][]float32

func (r embeddedRetriever) Retrieve(context.Context, string, *ragtypes.SearchOptions) ([]ragtypes.SearchHit, error) {
	var hits []ragtypes.SearchHit
	for _, id := range []string{"a", "b", "c"} {
		hits = append(hits, ragtypes.SearchHit{
			Variant: ragtypes.ContentVariant{UUID: id, ContentType: ragtypes.ContentText, Text: id, Embedding: r[id]},
			Score:   1,
		})
	}
	return hits, nil
}

// WithCrossEncoder and WithMMR together rank by relevance, then diversify
// over that order: b duplicates a, so the less relevant but distinct c
// comes before it.
func TestCrossEncoderThenMMR(t *testing.T) {
	pipe, err := rag.New(rag.Config{},
		rag.WithStore(memstore.New()),
		rag.WithContentExtractor(&stubExtractor{}),
		rag.WithRetrievers(embeddedRetriever{"a": {1, 0}, "b": {1, 0}, "c": {0, 1}}),
		rag.WithCrossEncoder(fixedScorer{"a": 0.9, "b": 0.8, "c": 0.1}),
		rag.WithMMR(0.5),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipe.Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range result.Hits {
		got = append(got, h.Variant.UUID)
	}
	if want := []string{"a", "c", "b"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}
