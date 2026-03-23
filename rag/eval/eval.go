// Package eval provides evaluation metrics for RAG pipelines.
package eval

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/urmzd/saige/rag/types"
)

// --- Types ---

// EvalResult holds computed evaluation metrics.
type EvalResult struct {
	// Retrieval metrics.
	ContextPrecision float64 `json:"context_precision"`
	ContextRecall    float64 `json:"context_recall"`
	NDCG             float64 `json:"ndcg"`
	MRR              float64 `json:"mrr"`
	HitRate          float64 `json:"hit_rate"`

	// Generation metrics.
	Faithfulness      float64 `json:"faithfulness,omitempty"`
	AnswerRelevancy   float64 `json:"answer_relevancy,omitempty"`
	AnswerCorrectness float64 `json:"answer_correctness,omitempty"`
	JudgeScore        float64 `json:"judge_score,omitempty"`
	JudgeReason       string  `json:"judge_reason,omitempty"`

	// Debuggability detail for faithfulness.
	FaithfulnessDetail *FaithfulnessDetail `json:"faithfulness_detail,omitempty"`

	// Latency tracking.
	RetrievalMs int64 `json:"retrieval_ms"`
	TotalMs     int64 `json:"total_ms"`
}

// ClaimVerdict is the verdict for a single atomic claim.
type ClaimVerdict struct {
	Claim     string `json:"claim"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// FaithfulnessDetail contains per-claim verdicts.
type FaithfulnessDetail struct {
	Claims []ClaimVerdict `json:"claims"`
}

// EvalCase defines a single evaluation case with ground truth.
type EvalCase struct {
	Query         string   `json:"query"`
	GroundTruth   string   `json:"ground_truth"`
	RelevantUUIDs []string `json:"relevant_uuids"`
	Response      string   `json:"response"`
}

// EvalOptions configures which metrics to compute and their parameters.
type EvalOptions struct {
	LLM                  types.LLM
	Embedders            types.EmbedderRegistry
	K                    int
	RelevancySampleCount int
	JudgeRubric          string
}

// EvalOption is a functional option for Evaluate.
type EvalOption func(*EvalOptions)

// WithLLM sets the LLM for generation metrics.
func WithLLM(llm types.LLM) EvalOption {
	return func(o *EvalOptions) { o.LLM = llm }
}

// WithEmbedders sets the embedder registry for relevancy metrics.
func WithEmbedders(e types.EmbedderRegistry) EvalOption {
	return func(o *EvalOptions) { o.Embedders = e }
}

// WithK sets the cutoff for NDCG@k and HitRate@k (default 10).
func WithK(k int) EvalOption {
	return func(o *EvalOptions) { o.K = k }
}

// WithRelevancySampleCount sets synthetic question count for AnswerRelevancy (default 3).
func WithRelevancySampleCount(n int) EvalOption {
	return func(o *EvalOptions) { o.RelevancySampleCount = n }
}

// WithJudgeRubric enables LLM-as-Judge with the given criteria rubric.
func WithJudgeRubric(rubric string) EvalOption {
	return func(o *EvalOptions) { o.JudgeRubric = rubric }
}

// --- Retrieval Metrics ---

// ContextPrecision computes Average Precision over relevant UUIDs.
func ContextPrecision(hits []types.SearchHit, relevantUUIDs []string) float64 {
	if len(relevantUUIDs) == 0 {
		return 0
	}

	relevant := make(map[string]bool, len(relevantUUIDs))
	for _, uuid := range relevantUUIDs {
		relevant[uuid] = true
	}

	sum := 0.0
	found := 0
	for i, hit := range hits {
		if relevant[hit.Variant.UUID] {
			found++
			sum += float64(found) / float64(i+1)
		}
	}

	if found == 0 {
		return 0
	}
	return sum / float64(len(relevantUUIDs))
}

// ContextRecall computes the fraction of relevant UUIDs present in the results.
func ContextRecall(hits []types.SearchHit, relevantUUIDs []string) float64 {
	if len(relevantUUIDs) == 0 {
		return 0
	}

	hitUUIDs := make(map[string]bool, len(hits))
	for _, hit := range hits {
		hitUUIDs[hit.Variant.UUID] = true
	}

	found := 0
	for _, uuid := range relevantUUIDs {
		if hitUUIDs[uuid] {
			found++
		}
	}

	return float64(found) / float64(len(relevantUUIDs))
}

// NDCG computes Normalized Discounted Cumulative Gain at rank k using binary relevance.
func NDCG(hits []types.SearchHit, relevantUUIDs []string, k int) float64 {
	if len(relevantUUIDs) == 0 || k <= 0 {
		return 0
	}

	relevant := make(map[string]bool, len(relevantUUIDs))
	for _, uuid := range relevantUUIDs {
		relevant[uuid] = true
	}

	n := min(k, len(hits))

	// DCG: sum of rel_i / log2(i+2) for 0-indexed i.
	dcg := 0.0
	for i := 0; i < n; i++ {
		if relevant[hits[i].Variant.UUID] {
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}

	// Ideal DCG: all relevant items at top positions.
	idealCount := min(k, len(relevantUUIDs))
	idcg := 0.0
	for i := 0; i < idealCount; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}

	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// MRR computes the Reciprocal Rank: 1/rank of the first relevant hit.
func MRR(hits []types.SearchHit, relevantUUIDs []string) float64 {
	if len(relevantUUIDs) == 0 {
		return 0
	}

	relevant := make(map[string]bool, len(relevantUUIDs))
	for _, uuid := range relevantUUIDs {
		relevant[uuid] = true
	}

	for i, hit := range hits {
		if relevant[hit.Variant.UUID] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// HitRate returns 1.0 if any relevant document appears in the top-k hits, else 0.0.
func HitRate(hits []types.SearchHit, relevantUUIDs []string, k int) float64 {
	if len(relevantUUIDs) == 0 || k <= 0 {
		return 0
	}

	relevant := make(map[string]bool, len(relevantUUIDs))
	for _, uuid := range relevantUUIDs {
		relevant[uuid] = true
	}

	n := min(k, len(hits))
	for i := 0; i < n; i++ {
		if relevant[hits[i].Variant.UUID] {
			return 1.0
		}
	}
	return 0
}

// --- Generation Metrics ---

// Prompt templates are loaded via //go:embed in embed.go.

// Faithfulness decomposes the response into atomic claims and verifies each against context.
func Faithfulness(ctx context.Context, response string, contextText string, llm types.LLM) (float64, *FaithfulnessDetail, error) {
	// Step 1: Decompose into claims.
	decomposeResult, err := llm.Generate(ctx, renderPrompt(faithfulnessDecomposeTmpl, map[string]any{"Response": response}))
	if err != nil {
		return 0, nil, fmt.Errorf("faithfulness decompose: %w", err)
	}

	claims := parseLines(decomposeResult)
	if len(claims) == 0 {
		return 0, &FaithfulnessDetail{}, nil
	}

	// Step 2: Verify claims against context.
	claimsList := strings.Join(claims, "\n")
	verifyResult, err := llm.Generate(ctx, renderPrompt(faithfulnessVerifyTmpl, map[string]any{"Context": contextText, "Claims": claimsList}))
	if err != nil {
		return 0, nil, fmt.Errorf("faithfulness verify: %w", err)
	}

	verdictLines := parseLines(verifyResult)
	verdicts := make([]ClaimVerdict, len(claims))
	supported := 0

	for i, claim := range claims {
		v := ClaimVerdict{Claim: claim}
		if i < len(verdictLines) {
			verdict, reason := parseVerdict(verdictLines[i])
			v.Supported = verdict
			v.Reason = reason
		}
		if v.Supported {
			supported++
		}
		verdicts[i] = v
	}

	score := float64(supported) / float64(len(claims))
	return score, &FaithfulnessDetail{Claims: verdicts}, nil
}

// AnswerRelevancy generates synthetic questions from the response, embeds them, and
// computes average cosine similarity to the original query embedding.
func AnswerRelevancy(ctx context.Context, query, response string, llm types.LLM, embedders types.EmbedderRegistry, sampleCount int) (float64, error) {
	if sampleCount <= 0 {
		sampleCount = 3
	}

	result, err := llm.Generate(ctx, renderPrompt(answerRelevancyTmpl, map[string]any{"Count": sampleCount, "Response": response}))
	if err != nil {
		return 0, fmt.Errorf("answer relevancy generate: %w", err)
	}

	questions := parseLines(result)
	if len(questions) == 0 {
		return 0, nil
	}

	// Build variants: query first, then synthetic questions.
	variants := make([]types.ContentVariant, 0, 1+len(questions))
	variants = append(variants, types.ContentVariant{ContentType: types.ContentText, Text: query})
	for _, q := range questions {
		variants = append(variants, types.ContentVariant{ContentType: types.ContentText, Text: q})
	}

	embeddings, err := embedders.Embed(ctx, variants)
	if err != nil {
		return 0, fmt.Errorf("answer relevancy embed: %w", err)
	}

	if len(embeddings) < 2 {
		return 0, nil
	}

	queryEmb := embeddings[0]
	sum := 0.0
	for i := 1; i < len(embeddings); i++ {
		sum += cosineSimilarity(queryEmb, embeddings[i])
	}
	return sum / float64(len(embeddings)-1), nil
}

// AnswerCorrectness uses an LLM to compare the generated answer against ground truth.
func AnswerCorrectness(ctx context.Context, response, groundTruth string, llm types.LLM) (float64, error) {
	result, err := llm.Generate(ctx, renderPrompt(answerCorrectnessTmpl, map[string]any{"GroundTruth": groundTruth, "Response": response}))
	if err != nil {
		return 0, fmt.Errorf("answer correctness: %w", err)
	}

	return parseScoreLine(result)
}

// LLMJudge performs pointwise scoring using a customizable criteria rubric.
func LLMJudge(ctx context.Context, query, response, contextText, rubric string, llm types.LLM) (float64, string, error) {
	result, err := llm.Generate(ctx, renderPrompt(llmJudgeTmpl, map[string]any{"Query": query, "Context": contextText, "Response": response, "Rubric": rubric}))
	if err != nil {
		return 0, "", fmt.Errorf("llm judge: %w", err)
	}

	score, err := parseScoreLine(result)
	if err != nil {
		return 0, "", err
	}
	reason := parseReasonLine(result)
	return score, reason, nil
}

// --- Orchestrator ---

// Evaluate runs all cases through the pipeline and computes all applicable metrics.
func Evaluate(ctx context.Context, cases []EvalCase, pipe types.Pipeline, opts ...EvalOption) ([]EvalResult, error) {
	o := &EvalOptions{K: 10, RelevancySampleCount: 3}
	for _, opt := range opts {
		opt(o)
	}

	results := make([]EvalResult, len(cases))

	for i, tc := range cases {
		totalStart := time.Now()

		// Retrieval phase.
		retrievalStart := time.Now()
		sr, err := pipe.Search(ctx, tc.Query, types.WithLimit(max(20, o.K)))
		if err != nil {
			return nil, fmt.Errorf("evaluate case %d: %w", i, err)
		}
		results[i].RetrievalMs = time.Since(retrievalStart).Milliseconds()

		// Retrieval metrics.
		results[i].ContextPrecision = ContextPrecision(sr.Hits, tc.RelevantUUIDs)
		results[i].ContextRecall = ContextRecall(sr.Hits, tc.RelevantUUIDs)
		results[i].NDCG = NDCG(sr.Hits, tc.RelevantUUIDs, o.K)
		results[i].MRR = MRR(sr.Hits, tc.RelevantUUIDs)
		results[i].HitRate = HitRate(sr.Hits, tc.RelevantUUIDs, o.K)

		// Generation metrics.
		if o.LLM != nil && tc.Response != "" {
			contextText := buildContextText(sr.Hits)

			faith, detail, err := Faithfulness(ctx, tc.Response, contextText, o.LLM)
			if err == nil {
				results[i].Faithfulness = faith
				results[i].FaithfulnessDetail = detail
			}

			if o.Embedders != nil {
				rel, err := AnswerRelevancy(ctx, tc.Query, tc.Response, o.LLM, o.Embedders, o.RelevancySampleCount)
				if err == nil {
					results[i].AnswerRelevancy = rel
				}
			}

			if tc.GroundTruth != "" {
				correctness, err := AnswerCorrectness(ctx, tc.Response, tc.GroundTruth, o.LLM)
				if err == nil {
					results[i].AnswerCorrectness = correctness
				}
			}

			if o.JudgeRubric != "" {
				score, reason, err := LLMJudge(ctx, tc.Query, tc.Response, contextText, o.JudgeRubric, o.LLM)
				if err == nil {
					results[i].JudgeScore = score
					results[i].JudgeReason = reason
				}
			}
		}

		results[i].TotalMs = time.Since(totalStart).Milliseconds()
	}

	return results, nil
}

// --- Helpers ---

func buildContextText(hits []types.SearchHit) string {
	var parts []string
	for _, hit := range hits {
		parts = append(parts, hit.Variant.Text)
	}
	return strings.Join(parts, "\n\n")
}

func parseLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseVerdict(line string) (supported bool, reason string) {
	parts := strings.SplitN(line, "|", 2)
	verdict := strings.TrimSpace(strings.ToLower(parts[0]))
	if len(parts) > 1 {
		reason = strings.TrimSpace(parts[1])
	}
	return verdict == "supported", reason
}

func parseScoreLine(output string) (float64, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "SCORE:") {
			scoreStr := strings.TrimSpace(line[len("SCORE:"):])
			var score float64
			_, err := fmt.Sscanf(scoreStr, "%f", &score)
			if err != nil {
				return 0, fmt.Errorf("parse score %q: %w", scoreStr, err)
			}
			return math.Max(0, math.Min(1, score)), nil
		}
	}
	return 0, fmt.Errorf("no SCORE: line found in output")
}

func parseReasonLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "REASONING:") {
			return strings.TrimSpace(line[len("REASONING:"):])
		}
		if strings.HasPrefix(upper, "REASON:") {
			return strings.TrimSpace(line[len("REASON:"):])
		}
	}
	return ""
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
