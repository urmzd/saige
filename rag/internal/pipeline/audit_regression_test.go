package pipeline_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// rescoringReranker replaces each hit's score from a table and sorts, as a
// cross-encoder does.
type rescoringReranker map[string]float64

func (r rescoringReranker) Rerank(_ context.Context, _ string, hits []types.SearchHit) ([]types.SearchHit, error) {
	out := slices.Clone(hits)
	for i := range out {
		out[i].Score = r[out[i].Variant.UUID]
	}
	slices.SortStableFunc(out, func(a, b types.SearchHit) int { return cmp.Compare(b.Score, a.Score) })
	return out, nil
}

func (rescoringReranker) ReplacesScores() bool { return true }

func recencyOrder(t *testing.T, scores rescoringReranker) []string {
	t.Helper()
	old, fresh := textHit("old"), textHit("fresh")
	old.Timestamp = time.Now().Add(-30 * 24 * time.Hour)
	fresh.Timestamp = time.Now()
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{&scriptedRetriever{hits: []types.SearchHit{old, fresh}}},
		Reranker:         scores,
	})
	result, err := pipe.Search(context.Background(), "q", types.WithRecency(24*time.Hour, 1))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range result.Hits {
		got = append(got, h.Variant.UUID)
	}
	return got
}

// A reranker that replaces scores drops the recency blended into the fused
// scores, so the pipeline applies it again.
func TestPipelineRecencyAppliesAfterScoringReranker(t *testing.T) {
	if got := recencyOrder(t, rescoringReranker{"old": 0.9, "fresh": 0.8}); !slices.Equal(got, []string{"fresh", "old"}) {
		t.Errorf("order = %v, want the month-old hit decayed below the fresh one", got)
	}
}

// Recency must lower a negative score, not pull it up toward zero.
func TestPipelineRecencyLowersNegativeScores(t *testing.T) {
	if got := recencyOrder(t, rescoringReranker{"old": -1.0, "fresh": -1.2}); !slices.Equal(got, []string{"fresh", "old"}) {
		t.Errorf("order = %v, want the month-old hit decayed below the fresh one", got)
	}
}

// partialAssembler returns a usable context together with a partial error.
type partialAssembler struct{}

func (partialAssembler) Assemble(_ context.Context, _ string, hits []types.SearchHit) (*types.AssembledContext, error) {
	return &types.AssembledContext{Prompt: "ctx", Blocks: []types.ContextBlock{{Text: hits[0].Variant.Text}}},
		fmt.Errorf("%w: one compression call failed", types.ErrPartialSearch)
}

func TestPipelinePartialAssemblyIsKept(t *testing.T) {
	pipe := pipeline.New(pipeline.Config{
		Store:            memstore.New(),
		ContentExtractor: &simpleExtractor{},
		Retrievers:       []types.Retriever{&scriptedRetriever{hits: []types.SearchHit{textHit("a")}}},
		ContextAssembler: partialAssembler{},
	})
	result, err := pipe.Search(context.Background(), "q", types.WithContextAssembly(0))
	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("err = %v, want partial", err)
	}
	if result == nil || result.Context == nil || len(result.Context.Blocks) != 1 {
		t.Fatalf("result = %+v, want the partial context kept", result)
	}
}
