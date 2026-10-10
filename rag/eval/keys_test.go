package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	topeval "github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/eval"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

func provHits(pairs ...[2]string) []types.SearchHit {
	h := make([]types.SearchHit, len(pairs))
	for i, p := range pairs {
		h[i] = types.SearchHit{
			Variant:    types.ContentVariant{UUID: p[0]},
			Provenance: types.Provenance{DocumentUUID: p[1], SourceURI: "file://" + p[1]},
		}
	}
	return h
}

func TestScoreRetrieval(t *testing.T) {
	tests := []struct {
		name      string
		hits      []types.SearchHit
		relevant  []string
		key       eval.RelevanceKey
		k         int
		want      eval.RetrievalScores
		wantLabel bool
	}{
		{
			name:     "no labels is no signal",
			hits:     hits("a"),
			relevant: nil,
			key:      eval.RelevanceVariant,
		},
		{
			name:      "duplicate labels count once",
			hits:      hits("a"),
			relevant:  []string{"a", "a"},
			key:       eval.RelevanceVariant,
			want:      eval.RetrievalScores{ContextPrecision: 1, ContextRecall: 1, NDCG: 1, MRR: 1, HitRate: 1},
			wantLabel: true,
		},
		{
			name:      "cutoff applies to recall",
			hits:      hits("a", "b", "c"),
			relevant:  []string{"c"},
			key:       eval.RelevanceVariant,
			k:         2,
			wantLabel: true,
		},
		{
			name: "document key ranks each document once",
			// d1 appears twice; d2 is second once duplicates collapse.
			hits:      provHits([2]string{"v1", "d1"}, [2]string{"v2", "d1"}, [2]string{"v3", "d2"}),
			relevant:  []string{"d2"},
			key:       eval.RelevanceDocument,
			k:         2,
			want:      eval.RetrievalScores{ContextPrecision: 0.5, ContextRecall: 1, NDCG: 0.6309, MRR: 0.5, HitRate: 1},
			wantLabel: true,
		},
		{
			name:      "source key",
			hits:      provHits([2]string{"v1", "d1"}),
			relevant:  []string{"file://d1"},
			key:       eval.RelevanceSource,
			want:      eval.RetrievalScores{ContextPrecision: 1, ContextRecall: 1, NDCG: 1, MRR: 1, HitRate: 1},
			wantLabel: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, labeled := eval.ScoreRetrieval(tt.hits, tt.relevant, tt.key, tt.k)
			if labeled != tt.wantLabel {
				t.Fatalf("labeled = %v, want %v", labeled, tt.wantLabel)
			}
			assertClose(t, "precision", got.ContextPrecision, tt.want.ContextPrecision, 0.001)
			assertClose(t, "recall", got.ContextRecall, tt.want.ContextRecall, 0.001)
			assertClose(t, "ndcg", got.NDCG, tt.want.NDCG, 0.001)
			assertClose(t, "mrr", got.MRR, tt.want.MRR, 0.001)
			assertClose(t, "hit rate", got.HitRate, tt.want.HitRate, 0.001)
		})
	}
}

func TestContextPrecisionDuplicateLabels(t *testing.T) {
	assertClose(t, "precision", eval.ContextPrecision(hits("a"), []string{"a", "a"}), 1.0, 0.001)
}

func TestRetrievalScorersSkipUnlabeled(t *testing.T) {
	hitsJSON, _ := json.Marshal(provHits([2]string{"v1", "d1"}))
	tests := []struct {
		name        string
		annotations map[string]json.RawMessage
		wantName    string
	}{
		{
			name:        "no labels",
			annotations: map[string]json.RawMessage{eval.AnnotationHits: hitsJSON},
		},
		{
			name: "document keys",
			annotations: map[string]json.RawMessage{
				eval.AnnotationHits:         hitsJSON,
				eval.AnnotationRelevantKeys: json.RawMessage(`["d1"]`),
				eval.AnnotationRelevanceKey: json.RawMessage(`"document"`),
			},
			wantName: "context_recall",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, err := eval.ContextRecallScorer().Score(context.Background(), topeval.Observation{Annotations: tt.annotations})
			if err != nil {
				t.Fatal(err)
			}
			if score.Name != tt.wantName {
				t.Errorf("score name = %q, want %q", score.Name, tt.wantName)
			}
			if tt.wantName != "" && score.Value != 1 {
				t.Errorf("score = %v, want 1", score.Value)
			}
		})
	}
}

// scriptedSearchPipeline returns one scripted result and error per call.
type scriptedSearchPipeline struct {
	mockPipeline
	results []*types.SearchPipelineResult
	errs    []error
	calls   int
	limits  []int
}

func (p *scriptedSearchPipeline) Search(_ context.Context, _ string, opts ...types.SearchOption) (*types.SearchPipelineResult, error) {
	cfg := types.SearchConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	p.limits = append(p.limits, cfg.Limit)
	i := p.calls
	p.calls++
	return p.results[i], p.errs[i]
}

func TestEvaluatePartialSearchAndLabels(t *testing.T) {
	result := &types.SearchPipelineResult{Hits: provHits([2]string{"v1", "d1"})}
	pipe := &scriptedSearchPipeline{
		results: []*types.SearchPipelineResult{result, result, result},
		errs:    []error{nil, fmt.Errorf("%w: bm25 down", types.ErrPartialSearch), nil},
	}
	cases := []eval.EvalCase{
		{Query: "q1", RelevantUUIDs: []string{"v1"}},
		{Query: "q2", RelevantKeys: []string{"d1"}, RelevanceKey: eval.RelevanceDocument},
		{Query: "q3"},
	}
	got, err := eval.Evaluate(context.Background(), cases, pipe, eval.WithK(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	if got[0].RetrievalWarning != "" || got[1].RetrievalWarning == "" {
		t.Errorf("warnings = %q, %q; want only case 2 degraded", got[0].RetrievalWarning, got[1].RetrievalWarning)
	}
	assertClose(t, "case 1 recall", got[0].ContextRecall, 1, 0.001)
	assertClose(t, "case 2 recall", got[1].ContextRecall, 1, 0.001)
	if got[0].Unlabeled || got[1].Unlabeled || !got[2].Unlabeled {
		t.Errorf("unlabeled = %v %v %v, want only case 3", got[0].Unlabeled, got[1].Unlabeled, got[2].Unlabeled)
	}
	for i, l := range pipe.limits {
		if l != 3 {
			t.Errorf("case %d searched with limit %d, want K=3", i+1, l)
		}
	}
}

func TestEvaluateFatalSearchError(t *testing.T) {
	pipe := &scriptedSearchPipeline{
		results: []*types.SearchPipelineResult{nil},
		errs:    []error{errors.New("store down")},
	}
	if _, err := eval.Evaluate(context.Background(), []eval.EvalCase{{Query: "q"}}, pipe); err == nil {
		t.Fatal("want an error for a failed search")
	}
}

// freshExtractor assigns new UUIDs on every extraction, as the built-in
// extractors do.
type freshExtractor struct{ n int }

func (e *freshExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	e.n++
	doc, sec, v := fmt.Sprintf("d%d", e.n), fmt.Sprintf("s%d", e.n), fmt.Sprintf("v%d", e.n)
	return &types.Document{
		UUID: doc, SourceURI: raw.SourceURI,
		Sections: []types.Section{{UUID: sec, DocumentUUID: doc, Variants: []types.ContentVariant{
			{UUID: v, SectionUUID: sec, ContentType: types.ContentText, Text: string(raw.Data)},
		}}},
	}, nil
}

func TestEvaluateSourceLabelsSurviveReingest(t *testing.T) {
	ctx := context.Background()
	pipe, err := rag.New(rag.Config{}, rag.WithStore(memstore.New()), rag.WithContentExtractor(&freshExtractor{}), rag.WithBM25(nil), rag.WithDedupBehavior(types.DedupReplace))
	if err != nil {
		t.Fatal(err)
	}
	raw := &types.RawDocument{SourceURI: "file://zebra.md", Data: []byte("the zebra runs fast")}
	first, err := pipe.Ingest(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Ingest(ctx, raw); err != nil {
		t.Fatal(err)
	}

	cases := []eval.EvalCase{
		{Query: "zebra", RelevantKeys: []string{"file://zebra.md"}, RelevanceKey: eval.RelevanceSource},
		{Query: "zebra", RelevantKeys: []string{first.DocumentUUID}, RelevanceKey: eval.RelevanceDocument},
	}
	got, err := eval.Evaluate(ctx, cases, pipe)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "source-labeled recall", got[0].ContextRecall, 1, 0.001)
	// The replaced document has a new UUID, so a label naming the old one
	// no longer matches.
	assertClose(t, "stale document label recall", got[1].ContextRecall, 0, 0.001)
}

func TestRelevanceKeyValidation(t *testing.T) {
	result := &types.SearchPipelineResult{Hits: provHits([2]string{"v1", "d1"})}
	hitsJSON, _ := json.Marshal(result.Hits)
	tests := []struct {
		name    string
		key     eval.RelevanceKey
		wantErr bool
	}{
		{name: "empty means variant", key: ""},
		{name: "variant", key: eval.RelevanceVariant},
		{name: "section", key: eval.RelevanceSection},
		{name: "document", key: eval.RelevanceDocument},
		{name: "source", key: eval.RelevanceSource},
		{name: "abbreviation", key: "doc", wantErr: true},
		{name: "field name", key: "source_uri", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.key.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, want error %v", err, tt.wantErr)
			}

			pipe := &scriptedSearchPipeline{
				results: []*types.SearchPipelineResult{result},
				errs:    []error{nil},
			}
			cases := []eval.EvalCase{{Query: "q", RelevantKeys: []string{"d1"}, RelevanceKey: tt.key}}
			_, err := eval.Evaluate(context.Background(), cases, pipe)
			if tt.wantErr != errors.Is(err, eval.ErrUnknownRelevanceKey) || (!tt.wantErr && err != nil) {
				t.Errorf("Evaluate err = %v, want unknown key error %v", err, tt.wantErr)
			}
			if tt.wantErr && pipe.calls != 0 {
				t.Errorf("searched %d times, want validation before any search", pipe.calls)
			}

			keyJSON, _ := json.Marshal(tt.key)
			obs := topeval.Observation{Annotations: map[string]json.RawMessage{
				eval.AnnotationHits:         hitsJSON,
				eval.AnnotationRelevantKeys: json.RawMessage(`["d1"]`),
				eval.AnnotationRelevanceKey: keyJSON,
			}}
			_, err = eval.ContextRecallScorer().Score(context.Background(), obs)
			if tt.wantErr != errors.Is(err, eval.ErrUnknownRelevanceKey) || (!tt.wantErr && err != nil) {
				t.Errorf("scorer err = %v, want unknown key error %v", err, tt.wantErr)
			}
		})
	}
}
