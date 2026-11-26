package reranker_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/rag/reranker"
	"github.com/urmzd/saige/rag/types"
)

// appendReranker records its name on each call and reverses the hits.
type appendReranker struct {
	name   string
	calls  *[]string
	err    error
	scores bool
}

func (r appendReranker) Rerank(_ context.Context, _ string, hits []types.SearchHit) ([]types.SearchHit, error) {
	*r.calls = append(*r.calls, r.name)
	if r.err != nil {
		return nil, r.err
	}
	out := slices.Clone(hits)
	slices.Reverse(out)
	return out, nil
}

func (r appendReranker) ReplacesScores() bool { return r.scores }

func TestChainRunsInOrderAndStopsAtError(t *testing.T) {
	var calls []string
	hits := []types.SearchHit{{Variant: types.ContentVariant{UUID: "a"}}, {Variant: types.ContentVariant{UUID: "b"}}}

	c := reranker.Chain{appendReranker{name: "first", calls: &calls}, appendReranker{name: "second", calls: &calls}}
	got, err := c.Rerank(context.Background(), "q", hits)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"first", "second"}) || got[0].Variant.UUID != "a" {
		t.Errorf("calls = %v, first hit = %s; want both stages in order, each over the last", calls, got[0].Variant.UUID)
	}

	calls = nil
	boom := errors.New("boom")
	c = reranker.Chain{appendReranker{name: "first", calls: &calls, err: boom}, appendReranker{name: "second", calls: &calls}}
	if _, err := c.Rerank(context.Background(), "q", hits); !errors.Is(err, boom) || len(calls) != 1 {
		t.Errorf("err = %v, calls = %v; want the first error and no later stage", err, calls)
	}
}

// Recency is reapplied only when the final order comes from a scoring
// stage; with MMR last the pipeline must not re-sort by score.
func TestChainReplacesScoresFollowsLastStage(t *testing.T) {
	var calls []string
	scoring := appendReranker{calls: &calls, scores: true}
	tests := []struct {
		name  string
		chain reranker.Chain
		want  bool
	}{
		{"empty", reranker.Chain{}, false},
		{"scoring last", reranker.Chain{reranker.NewMMR(0.5), scoring}, true},
		{"mmr last", reranker.Chain{scoring, reranker.NewMMR(0.5)}, false},
		{"last stage keeps scores", reranker.Chain{scoring, appendReranker{calls: &calls}}, false},
	}
	for _, tt := range tests {
		if got := tt.chain.ReplacesScores(); got != tt.want {
			t.Errorf("%s: ReplacesScores = %v, want %v", tt.name, got, tt.want)
		}
	}
}
