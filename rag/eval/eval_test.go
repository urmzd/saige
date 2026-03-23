package eval_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/urmzd/saige/rag/eval"
	"github.com/urmzd/saige/rag/types"
)

// --- Mocks ---

type mockLLM struct {
	responses []string
	callIndex int
}

func (m *mockLLM) Generate(_ context.Context, _ string) (string, error) {
	if len(m.responses) == 0 {
		return "", fmt.Errorf("no responses configured")
	}
	resp := m.responses[m.callIndex%len(m.responses)]
	m.callIndex++
	return resp, nil
}

type mockEmbedderRegistry struct {
	embeddings [][]float32
}

func (m *mockEmbedderRegistry) Register(_ types.ContentType, _ types.VariantEmbedder) {}
func (m *mockEmbedderRegistry) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	result := make([][]float32, len(variants))
	for i := range variants {
		if i < len(m.embeddings) {
			result[i] = m.embeddings[i]
		} else {
			result[i] = m.embeddings[len(m.embeddings)-1]
		}
	}
	return result, nil
}

type mockPipeline struct {
	searchResult *types.SearchPipelineResult
}

func (m *mockPipeline) Ingest(_ context.Context, _ *types.RawDocument) (*types.IngestResult, error) {
	return nil, nil
}
func (m *mockPipeline) Search(_ context.Context, _ string, _ ...types.SearchOption) (*types.SearchPipelineResult, error) {
	return m.searchResult, nil
}
func (m *mockPipeline) Lookup(_ context.Context, _ string) (*types.SearchHit, error) { return nil, nil }
func (m *mockPipeline) Update(_ context.Context, _ string, _ *types.RawDocument) (*types.IngestResult, error) {
	return nil, nil
}
func (m *mockPipeline) Delete(_ context.Context, _ string) error            { return nil }
func (m *mockPipeline) Reconstruct(_ context.Context, _ string) (*types.Document, error) {
	return nil, nil
}
func (m *mockPipeline) Close(_ context.Context) error { return nil }

// --- Helpers ---

func hits(uuids ...string) []types.SearchHit {
	h := make([]types.SearchHit, len(uuids))
	for i, uuid := range uuids {
		h[i] = types.SearchHit{Variant: types.ContentVariant{UUID: uuid}}
	}
	return h
}

func assertClose(t *testing.T, name string, got, want, epsilon float64) {
	t.Helper()
	if math.Abs(got-want) > epsilon {
		t.Errorf("%s: got %.4f, want %.4f (±%.4f)", name, got, want, epsilon)
	}
}

// --- ContextPrecision Tests ---

func TestContextPrecision(t *testing.T) {
	h := hits("a", "b", "c", "d")
	// Relevant at positions 0 and 2.
	// AP = (1/1 + 2/3) / 2 = 0.833
	expected := (1.0 + 2.0/3.0) / 2.0
	assertClose(t, "precision", eval.ContextPrecision(h, []string{"a", "c"}), expected, 0.001)
}

func TestContextPrecisionPerfect(t *testing.T) {
	assertClose(t, "perfect", eval.ContextPrecision(hits("a", "b"), []string{"a", "b"}), 1.0, 0.001)
}

func TestContextPrecisionNoHits(t *testing.T) {
	assertClose(t, "no hits", eval.ContextPrecision(nil, []string{"a"}), 0, 0.001)
}

func TestContextPrecisionNoRelevant(t *testing.T) {
	assertClose(t, "no relevant", eval.ContextPrecision(hits("a"), []string{}), 0, 0.001)
}

// --- ContextRecall Tests ---

func TestContextRecall(t *testing.T) {
	assertClose(t, "recall", eval.ContextRecall(hits("a", "b", "c"), []string{"a", "c", "d"}), 2.0/3.0, 0.001)
}

func TestContextRecallPerfect(t *testing.T) {
	assertClose(t, "perfect", eval.ContextRecall(hits("a", "b"), []string{"a", "b"}), 1.0, 0.001)
}

func TestContextRecallNoRelevant(t *testing.T) {
	assertClose(t, "no relevant", eval.ContextRecall(hits("a"), []string{}), 0, 0.001)
}

// --- NDCG Tests ---

func TestNDCGPerfect(t *testing.T) {
	// All relevant at top.
	assertClose(t, "perfect", eval.NDCG(hits("a", "b", "c"), []string{"a", "b"}, 3), 1.0, 0.001)
}

func TestNDCGInverted(t *testing.T) {
	// Relevant items at positions 2,3 instead of 0,1.
	h := hits("x", "y", "a", "b")
	got := eval.NDCG(h, []string{"a", "b"}, 4)
	// DCG = 1/log2(4) + 1/log2(5) = 0.5 + 0.431 = 0.931
	// IDCG = 1/log2(2) + 1/log2(3) = 1.0 + 0.631 = 1.631
	expected := (1.0/math.Log2(4) + 1.0/math.Log2(5)) / (1.0/math.Log2(2) + 1.0/math.Log2(3))
	assertClose(t, "inverted", got, expected, 0.001)
}

func TestNDCGNoRelevant(t *testing.T) {
	assertClose(t, "no relevant", eval.NDCG(hits("a"), []string{}, 5), 0, 0.001)
}

func TestNDCGKLargerThanHits(t *testing.T) {
	assertClose(t, "k>len", eval.NDCG(hits("a", "b"), []string{"a"}, 10), 1.0, 0.001)
}

func TestNDCGKZero(t *testing.T) {
	assertClose(t, "k=0", eval.NDCG(hits("a"), []string{"a"}, 0), 0, 0.001)
}

func TestNDCGKOne(t *testing.T) {
	// Only look at first result. It's relevant → NDCG=1.
	assertClose(t, "k=1 relevant", eval.NDCG(hits("a", "b"), []string{"a"}, 1), 1.0, 0.001)
	// First result not relevant → NDCG=0.
	assertClose(t, "k=1 irrelevant", eval.NDCG(hits("x", "a"), []string{"a"}, 1), 0, 0.001)
}

// --- MRR Tests ---

func TestMRRFirstHit(t *testing.T) {
	assertClose(t, "first", eval.MRR(hits("a", "b"), []string{"a"}), 1.0, 0.001)
}

func TestMRRThirdHit(t *testing.T) {
	assertClose(t, "third", eval.MRR(hits("x", "y", "a"), []string{"a"}), 1.0/3.0, 0.001)
}

func TestMRRNoRelevant(t *testing.T) {
	assertClose(t, "none", eval.MRR(hits("x", "y"), []string{"a"}), 0, 0.001)
}

func TestMRREmpty(t *testing.T) {
	assertClose(t, "empty", eval.MRR(nil, []string{"a"}), 0, 0.001)
}

func TestMRRNoRelevantUUIDs(t *testing.T) {
	assertClose(t, "no uuids", eval.MRR(hits("a"), []string{}), 0, 0.001)
}

// --- HitRate Tests ---

func TestHitRateFound(t *testing.T) {
	assertClose(t, "found", eval.HitRate(hits("x", "a", "y"), []string{"a"}, 3), 1.0, 0.001)
}

func TestHitRateOutsideK(t *testing.T) {
	assertClose(t, "outside k", eval.HitRate(hits("x", "y", "a"), []string{"a"}, 2), 0, 0.001)
}

func TestHitRateEmpty(t *testing.T) {
	assertClose(t, "empty", eval.HitRate(nil, []string{"a"}, 5), 0, 0.001)
}

func TestHitRateKZero(t *testing.T) {
	assertClose(t, "k=0", eval.HitRate(hits("a"), []string{"a"}, 0), 0, 0.001)
}

// --- Faithfulness Tests ---

func TestFaithfulnessAllSupported(t *testing.T) {
	llm := &mockLLM{responses: []string{
		"The sky is blue.\nWater is wet.", // decompose
		"supported|matches context\nsupported|matches context", // verify
	}}

	score, detail, err := eval.Faithfulness(context.Background(), "response", "context", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "all supported", score, 1.0, 0.001)
	if len(detail.Claims) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(detail.Claims))
	}
	for _, c := range detail.Claims {
		if !c.Supported {
			t.Errorf("expected supported, got unsupported for %q", c.Claim)
		}
	}
}

func TestFaithfulnessHalfSupported(t *testing.T) {
	llm := &mockLLM{responses: []string{
		"Claim one.\nClaim two.",
		"supported|ok\nunsupported|not in context",
	}}

	score, detail, err := eval.Faithfulness(context.Background(), "response", "context", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "half", score, 0.5, 0.001)
	if !detail.Claims[0].Supported {
		t.Error("first claim should be supported")
	}
	if detail.Claims[1].Supported {
		t.Error("second claim should be unsupported")
	}
}

func TestFaithfulnessNoClaims(t *testing.T) {
	llm := &mockLLM{responses: []string{""}}

	score, detail, err := eval.Faithfulness(context.Background(), "response", "context", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "no claims", score, 0, 0.001)
	if len(detail.Claims) != 0 {
		t.Errorf("expected 0 claims, got %d", len(detail.Claims))
	}
}

func TestFaithfulnessMalformedVerdict(t *testing.T) {
	llm := &mockLLM{responses: []string{
		"A claim.",
		"gibberish without pipe", // malformed → unsupported
	}}

	score, _, err := eval.Faithfulness(context.Background(), "response", "context", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "malformed", score, 0, 0.001)
}

// --- AnswerRelevancy Tests ---

func TestAnswerRelevancy(t *testing.T) {
	llm := &mockLLM{responses: []string{"What is X?\nHow does Y work?"}}
	// Query embedding = [1,0], synthetic questions = [1,0] → similarity = 1.0
	emb := &mockEmbedderRegistry{embeddings: [][]float32{
		{1, 0}, {1, 0}, {1, 0},
	}}

	score, err := eval.AnswerRelevancy(context.Background(), "query", "response", llm, emb, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "identical", score, 1.0, 0.001)
}

func TestAnswerRelevancyOrthogonal(t *testing.T) {
	llm := &mockLLM{responses: []string{"Q1"}}
	// Query=[1,0], synthetic=[0,1] → cosine=0
	emb := &mockEmbedderRegistry{embeddings: [][]float32{
		{1, 0}, {0, 1},
	}}

	score, err := eval.AnswerRelevancy(context.Background(), "query", "response", llm, emb, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "orthogonal", score, 0, 0.001)
}

func TestAnswerRelevancyNoQuestions(t *testing.T) {
	llm := &mockLLM{responses: []string{""}}
	emb := &mockEmbedderRegistry{}

	score, err := eval.AnswerRelevancy(context.Background(), "query", "response", llm, emb, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "no questions", score, 0, 0.001)
}

// --- AnswerCorrectness Tests ---

func TestAnswerCorrectness(t *testing.T) {
	llm := &mockLLM{responses: []string{"REASON: Good match\nSCORE: 0.85"}}

	score, err := eval.AnswerCorrectness(context.Background(), "answer", "truth", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "correctness", score, 0.85, 0.001)
}

func TestAnswerCorrectnessMalformed(t *testing.T) {
	llm := &mockLLM{responses: []string{"no score here"}}

	_, err := eval.AnswerCorrectness(context.Background(), "answer", "truth", llm)
	if err == nil {
		t.Fatal("expected error for malformed output")
	}
}

func TestAnswerCorrectnessClamps(t *testing.T) {
	llm := &mockLLM{responses: []string{"SCORE: 1.5"}}

	score, err := eval.AnswerCorrectness(context.Background(), "answer", "truth", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "clamped", score, 1.0, 0.001)
}

// --- LLMJudge Tests ---

func TestLLMJudge(t *testing.T) {
	llm := &mockLLM{responses: []string{"REASONING: Clear and concise\nSCORE: 0.9"}}

	score, reason, err := eval.LLMJudge(context.Background(), "query", "response", "context", "rubric", llm)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "judge score", score, 0.9, 0.001)
	if reason != "Clear and concise" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestLLMJudgeMissingScore(t *testing.T) {
	llm := &mockLLM{responses: []string{"REASONING: something\nno score"}}

	_, _, err := eval.LLMJudge(context.Background(), "query", "response", "context", "rubric", llm)
	if err == nil {
		t.Fatal("expected error for missing SCORE")
	}
}

// --- Evaluate Integration Test ---

func TestEvaluate(t *testing.T) {
	pipe := &mockPipeline{
		searchResult: &types.SearchPipelineResult{
			Hits: []types.SearchHit{
				{Variant: types.ContentVariant{UUID: "a", Text: "chunk one"}},
				{Variant: types.ContentVariant{UUID: "b", Text: "chunk two"}},
			},
		},
	}

	llm := &mockLLM{responses: []string{
		// Faithfulness decompose
		"Claim A.",
		// Faithfulness verify
		"supported|ok",
		// AnswerRelevancy generate questions
		"What is it?",
		// AnswerCorrectness
		"REASON: good\nSCORE: 0.8",
		// LLMJudge
		"REASONING: fine\nSCORE: 0.7",
	}}

	emb := &mockEmbedderRegistry{embeddings: [][]float32{
		{1, 0}, {1, 0},
	}}

	results, err := eval.Evaluate(context.Background(),
		[]eval.EvalCase{{
			Query:         "test",
			GroundTruth:   "ground truth",
			RelevantUUIDs: []string{"a"},
			Response:      "response text",
		}},
		pipe,
		eval.WithLLM(llm),
		eval.WithEmbedders(emb),
		eval.WithK(5),
		eval.WithJudgeRubric("Be helpful"),
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]

	// Retrieval metrics.
	assertClose(t, "precision", r.ContextPrecision, 1.0, 0.001)
	assertClose(t, "ndcg", r.NDCG, 1.0, 0.001)
	assertClose(t, "mrr", r.MRR, 1.0, 0.001)
	assertClose(t, "hit_rate", r.HitRate, 1.0, 0.001)

	// Generation metrics should be populated.
	if r.Faithfulness == 0 && r.FaithfulnessDetail == nil {
		t.Error("expected faithfulness to be computed")
	}
	if r.AnswerCorrectness == 0 {
		t.Error("expected answer correctness to be computed")
	}
	if r.TotalMs < 0 {
		t.Error("expected non-negative total latency")
	}
}

func TestEvaluateNoLLMSkipsGeneration(t *testing.T) {
	pipe := &mockPipeline{
		searchResult: &types.SearchPipelineResult{
			Hits: hits("a", "b"),
		},
	}

	results, err := eval.Evaluate(context.Background(),
		[]eval.EvalCase{{
			Query:         "test",
			RelevantUUIDs: []string{"a"},
			Response:      "response",
		}},
		pipe,
	)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	// Retrieval metrics should still work.
	assertClose(t, "mrr", r.MRR, 1.0, 0.001)
	// Generation metrics should be zero.
	assertClose(t, "faithfulness", r.Faithfulness, 0, 0.001)
	assertClose(t, "relevancy", r.AnswerRelevancy, 0, 0.001)
}
