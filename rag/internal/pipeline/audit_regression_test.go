package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/rag/contextassembler"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

type failingReranker struct{}

func (failingReranker) Rerank(context.Context, string, []types.SearchHit) ([]types.SearchHit, error) {
	return nil, errors.New("cross-encoder unavailable")
}

func TestPipelineRerankFailureKeepsFusedOrder(t *testing.T) {
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{&scriptedRetriever{hits: []types.SearchHit{textHit("a"), textHit("b")}}},
		Reranker:         failingReranker{},
	})
	result, err := pipe.Search(context.Background(), "q")
	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("err = %v, want partial", err)
	}
	if result == nil || len(result.Hits) != 2 || result.Hits[0].Variant.UUID != "a" {
		t.Fatalf("want fused hits a, b; got %+v", result)
	}
}

// queryRecorder records the queries it is asked and reports Dense.
type queryRecorder struct {
	dense   bool
	mu      sync.Mutex
	queries []string
}

func (r *queryRecorder) Retrieve(_ context.Context, q string, _ *types.SearchOptions) ([]types.SearchHit, error) {
	r.mu.Lock()
	r.queries = append(r.queries, q)
	r.mu.Unlock()
	return []types.SearchHit{textHit("x")}, nil
}

func (r *queryRecorder) Dense() bool { return r.dense }

type hypotheticalTransformer struct{ fixedTransformer }

func (hypotheticalTransformer) HypotheticalQueries() bool { return true }

func TestPipelineHypotheticalQueriesOnlyReachDenseRetrievers(t *testing.T) {
	dense := &queryRecorder{dense: true}
	lexical := &queryRecorder{}
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		QueryTransformer: hypotheticalTransformer{fixedTransformer{queries: []string{"q", "made-up answer"}}},
		Retrievers:       []types.Retriever{dense, lexical},
	})
	result, err := pipe.Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(dense.queries) != 2 {
		t.Errorf("dense retriever got %q, want both queries", dense.queries)
	}
	if len(lexical.queries) != 1 || lexical.queries[0] != "q" {
		t.Errorf("lexical retriever got %q, want only the original query", lexical.queries)
	}
	if len(result.Retrievals) != 3 {
		t.Errorf("retrieval stats = %d, want 3", len(result.Retrievals))
	}
}

func TestPipelineMinScoreIgnoresEmptyLists(t *testing.T) {
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers: []types.Retriever{
			&scriptedRetriever{hits: []types.SearchHit{textHit("a"), textHit("b")}},
			&scriptedRetriever{},
		},
	})
	result, err := pipe.Search(context.Background(), "q", types.WithMinScore(0.6))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("got %d hits, want 2: an empty list must not raise the ceiling", len(result.Hits))
	}
}

func TestPipelinePerSearchBudgetReachesConfiguredAssembler(t *testing.T) {
	long := strings.Repeat("word ", 200)
	hits := []types.SearchHit{textHit("short"), textHit("x")}
	hits[1].Variant.Text = long
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{&scriptedRetriever{hits: hits}},
		ContextAssembler: &contextassembler.DefaultAssembler{MaxTokens: 100000},
	})
	result, err := pipe.Search(context.Background(), "q", types.WithContextAssembly(50))
	if err != nil {
		t.Fatal(err)
	}
	if result.Context == nil || result.Context.TokenCount > 50 || len(result.Context.Blocks) != 1 {
		t.Fatalf("context = %+v, want one block within 50 tokens", result.Context)
	}
}
