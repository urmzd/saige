// Package eval provides evaluation metrics for RAG pipelines.
package eval

import (
	"context"
	"errors"
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

	// Unlabeled reports that the case has no relevance labels, so the
	// retrieval metrics above were not computed. Exclude such cases from
	// retrieval averages instead of counting their zero scores.
	Unlabeled bool `json:"unlabeled,omitempty"`

	// RetrievalWarning holds the partial-failure error returned by the
	// pipeline search (for example one retriever failing while others
	// returned hits). The metrics were computed from the hits that came back.
	RetrievalWarning string `json:"retrieval_warning,omitempty"`

	// Generation metrics.
	Faithfulness      float64 `json:"faithfulness,omitempty"`
	AnswerRelevancy   float64 `json:"answer_relevancy,omitempty"`
	AnswerCorrectness float64 `json:"answer_correctness,omitempty"`
	JudgeScore        float64 `json:"judge_score,omitempty"`
	JudgeReason       string  `json:"judge_reason,omitempty"`

	// Debuggability detail for faithfulness.
	FaithfulnessDetail *FaithfulnessDetail `json:"faithfulness_detail,omitempty"`

	// MetricErrors records generation-metric failures by metric name
	// ("faithfulness", "answer_relevancy", "answer_correctness", "llm_judge").
	// An errored metric's score field stays at its zero value and must not be
	// interpreted as a computed score.
	MetricErrors map[string]string `json:"metric_errors,omitempty"`

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

func (r *EvalResult) recordMetricError(metric string, err error) {
	if r.MetricErrors == nil {
		r.MetricErrors = make(map[string]string)
	}
	r.MetricErrors[metric] = err.Error()
}

// tmplResponse is the template field that carries the response under judgment.
const tmplResponse = "Response"

// RelevanceKey selects which identifier of a hit a relevance label names.
type RelevanceKey string

const (
	// RelevanceVariant labels hits by Variant.UUID. Variant UUIDs are
	// assigned at ingest, so labels must be refreshed after re-ingesting.
	RelevanceVariant RelevanceKey = "variant"
	// RelevanceSection labels hits by Provenance.SectionUUID.
	RelevanceSection RelevanceKey = "section"
	// RelevanceDocument labels hits by Provenance.DocumentUUID.
	RelevanceDocument RelevanceKey = "document"
	// RelevanceSource labels hits by Provenance.SourceURI, which stays the
	// same when a document is re-ingested from the same source.
	RelevanceSource RelevanceKey = "source"
)

// ErrUnknownRelevanceKey is returned when a relevance key is not one of the
// RelevanceKey constants. Without this check such labels would never match a
// hit and every retrieval metric would score zero.
var ErrUnknownRelevanceKey = errors.New("eval: unknown relevance key")

// Validate reports whether k is empty (meaning variant) or one of the
// RelevanceKey constants, and returns ErrUnknownRelevanceKey otherwise.
func (k RelevanceKey) Validate() error {
	switch k {
	case "", RelevanceVariant, RelevanceSection, RelevanceDocument, RelevanceSource:
		return nil
	}
	return fmt.Errorf("%w %q: want %q, %q, %q, or %q", ErrUnknownRelevanceKey, string(k),
		RelevanceVariant, RelevanceSection, RelevanceDocument, RelevanceSource)
}

// EvalCase defines a single evaluation case with ground truth.
//
// Relevance labels come from RelevantKeys, interpreted by RelevanceKey, or,
// when RelevantKeys is empty, from RelevantUUIDs as variant UUIDs. For keys
// coarser than a variant, several hits can share one key; only the first hit
// with a given key counts, so each relevant item is ranked once.
type EvalCase struct {
	Query         string   `json:"query"`
	GroundTruth   string   `json:"ground_truth"`
	RelevantUUIDs []string `json:"relevant_uuids"`
	Response      string   `json:"response"`

	RelevantKeys []string     `json:"relevant_keys,omitempty"`
	RelevanceKey RelevanceKey `json:"relevance_key,omitempty"`
}

// labels returns the case's relevance labels and the key they name.
func (c EvalCase) labels() ([]string, RelevanceKey) {
	if len(c.RelevantKeys) > 0 {
		return c.RelevantKeys, c.RelevanceKey
	}
	return c.RelevantUUIDs, RelevanceVariant
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

// WithK sets the retrieval cutoff (default 10). Evaluate searches for k hits
// and computes every retrieval metric (precision, recall, NDCG, MRR, and hit
// rate) over those top k.
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

// RetrievalScores holds every retrieval metric for one ranked result.
type RetrievalScores struct {
	ContextPrecision float64 `json:"context_precision"`
	ContextRecall    float64 `json:"context_recall"`
	NDCG             float64 `json:"ndcg"`
	MRR              float64 `json:"mrr"`
	HitRate          float64 `json:"hit_rate"`
}

// ScoreRetrieval computes every retrieval metric for hits against relevant
// labels of the given key, over the top k hits (all hits when k <= 0). Hits
// are first reduced to the first occurrence of each key, and duplicate labels
// count once. It returns false, with zero scores, when relevant is empty:
// such a case carries no retrieval signal.
func ScoreRetrieval(hits []types.SearchHit, relevant []string, key RelevanceKey, k int) (RetrievalScores, bool) {
	rel := labelSet(relevant)
	if len(rel) == 0 {
		return RetrievalScores{}, false
	}
	ranked := rankedKeys(hits, key)
	cutoff := k
	if cutoff <= 0 {
		cutoff = len(ranked)
	}
	if len(ranked) > cutoff {
		ranked = ranked[:cutoff]
	}
	return RetrievalScores{
		ContextPrecision: averagePrecision(ranked, rel),
		ContextRecall:    recall(ranked, rel),
		NDCG:             ndcg(ranked, rel, cutoff),
		MRR:              reciprocalRank(ranked, rel),
		HitRate:          hitRate(ranked, rel, cutoff),
	}, true
}

// hitKey returns the identifier of hit that labels of the given key name.
func hitKey(hit types.SearchHit, key RelevanceKey) string {
	switch key {
	case RelevanceSection:
		return hit.Provenance.SectionUUID
	case RelevanceDocument:
		return hit.Provenance.DocumentUUID
	case RelevanceSource:
		return hit.Provenance.SourceURI
	default:
		return hit.Variant.UUID
	}
}

// rankedKeys maps hits to their keys, keeping the first occurrence of each.
// Hits without a key keep their rank position under an empty key that never
// matches a label.
func rankedKeys(hits []types.SearchHit, key RelevanceKey) []string {
	seen := make(map[string]bool, len(hits))
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		id := hitKey(hit, key)
		if id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		out = append(out, id)
	}
	return out
}

// labelSet returns the non-empty labels as a set.
func labelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, l := range labels {
		if l != "" {
			set[l] = true
		}
	}
	return set
}

func averagePrecision(ranked []string, rel map[string]bool) float64 {
	sum := 0.0
	found := 0
	for i, id := range ranked {
		if rel[id] {
			found++
			sum += float64(found) / float64(i+1)
		}
	}
	if found == 0 {
		return 0
	}
	return sum / float64(len(rel))
}

func recall(ranked []string, rel map[string]bool) float64 {
	found := 0
	for _, id := range ranked {
		if rel[id] {
			found++
		}
	}
	return float64(found) / float64(len(rel))
}

func ndcg(ranked []string, rel map[string]bool, k int) float64 {
	if k <= 0 {
		return 0
	}
	n := min(k, len(ranked))
	// DCG: sum of rel_i / log2(i+2) for 0-indexed i.
	dcg := 0.0
	for i := 0; i < n; i++ {
		if rel[ranked[i]] {
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}
	// Ideal DCG: all relevant items at top positions.
	idealCount := min(k, len(rel))
	idcg := 0.0
	for i := 0; i < idealCount; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func reciprocalRank(ranked []string, rel map[string]bool) float64 {
	for i, id := range ranked {
		if rel[id] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

func hitRate(ranked []string, rel map[string]bool, k int) float64 {
	n := min(k, len(ranked))
	for i := 0; i < n; i++ {
		if rel[ranked[i]] {
			return 1.0
		}
	}
	return 0
}

// ContextPrecision computes Average Precision over relevant variant UUIDs.
// Duplicate UUIDs count once. It returns 0 when relevantUUIDs is empty; use
// ScoreRetrieval to tell an unlabeled case from a miss.
func ContextPrecision(hits []types.SearchHit, relevantUUIDs []string) float64 {
	s, _ := ScoreRetrieval(hits, relevantUUIDs, RelevanceVariant, 0)
	return s.ContextPrecision
}

// ContextRecall computes the fraction of relevant variant UUIDs present in
// the results. Duplicate UUIDs count once.
func ContextRecall(hits []types.SearchHit, relevantUUIDs []string) float64 {
	s, _ := ScoreRetrieval(hits, relevantUUIDs, RelevanceVariant, 0)
	return s.ContextRecall
}

// NDCG computes Normalized Discounted Cumulative Gain at rank k using binary relevance.
func NDCG(hits []types.SearchHit, relevantUUIDs []string, k int) float64 {
	if k <= 0 {
		return 0
	}
	s, _ := ScoreRetrieval(hits, relevantUUIDs, RelevanceVariant, k)
	return s.NDCG
}

// MRR computes the Reciprocal Rank: 1/rank of the first relevant hit.
func MRR(hits []types.SearchHit, relevantUUIDs []string) float64 {
	s, _ := ScoreRetrieval(hits, relevantUUIDs, RelevanceVariant, 0)
	return s.MRR
}

// HitRate returns 1.0 if any relevant document appears in the top-k hits, else 0.0.
func HitRate(hits []types.SearchHit, relevantUUIDs []string, k int) float64 {
	if k <= 0 {
		return 0
	}
	s, _ := ScoreRetrieval(hits, relevantUUIDs, RelevanceVariant, k)
	return s.HitRate
}

// --- Generation Metrics ---

// Prompt templates are loaded via //go:embed in embed.go.

// Faithfulness decomposes the response into atomic claims and verifies each against context.
func Faithfulness(ctx context.Context, response string, contextText string, llm types.LLM) (float64, *FaithfulnessDetail, error) {
	// Step 1: Decompose into claims.
	decomposeResult, err := llm.Generate(ctx, renderPrompt(faithfulnessDecomposeTmpl, map[string]any{tmplResponse: response}))
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

	result, err := llm.Generate(ctx, renderPrompt(answerRelevancyTmpl, map[string]any{"Count": sampleCount, tmplResponse: response}))
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
	result, err := llm.Generate(ctx, renderPrompt(answerCorrectnessTmpl, map[string]any{"GroundTruth": groundTruth, tmplResponse: response}))
	if err != nil {
		return 0, fmt.Errorf("answer correctness: %w", err)
	}

	return parseScoreLine(result)
}

// LLMJudge performs pointwise scoring using a customizable criteria rubric.
func LLMJudge(ctx context.Context, query, response, contextText, rubric string, llm types.LLM) (float64, string, error) {
	result, err := llm.Generate(ctx, renderPrompt(llmJudgeTmpl, map[string]any{"Query": query, "Context": contextText, tmplResponse: response, "Rubric": rubric}))
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

// Evaluate runs all cases through the pipeline and computes all applicable
// metrics. It returns an error wrapping ErrUnknownRelevanceKey, before any
// search runs, when a case names an unknown RelevanceKey.
func Evaluate(ctx context.Context, cases []EvalCase, pipe types.Pipeline, opts ...EvalOption) ([]EvalResult, error) {
	for i, tc := range cases {
		if err := tc.RelevanceKey.Validate(); err != nil {
			return nil, fmt.Errorf("evaluate case %d: %w", i, err)
		}
	}
	o := &EvalOptions{K: 10, RelevancySampleCount: 3}
	for _, opt := range opts {
		opt(o)
	}
	if o.K <= 0 {
		o.K = 10
	}

	results := make([]EvalResult, len(cases))

	for i, tc := range cases {
		totalStart := time.Now()

		// Retrieval phase.
		retrievalStart := time.Now()
		sr, err := pipe.Search(ctx, tc.Query, types.WithLimit(o.K))
		if err != nil {
			// A partial failure still returns hits; score them and keep the
			// warning. Any other error aborts the run.
			if !errors.Is(err, types.ErrPartialSearch) || sr == nil {
				return nil, fmt.Errorf("evaluate case %d: %w", i, err)
			}
			results[i].RetrievalWarning = err.Error()
		}
		results[i].RetrievalMs = time.Since(retrievalStart).Milliseconds()

		// Retrieval metrics over the top K.
		relevant, key := tc.labels()
		scores, labeled := ScoreRetrieval(sr.Hits, relevant, key, o.K)
		results[i].Unlabeled = !labeled
		results[i].ContextPrecision = scores.ContextPrecision
		results[i].ContextRecall = scores.ContextRecall
		results[i].NDCG = scores.NDCG
		results[i].MRR = scores.MRR
		results[i].HitRate = scores.HitRate

		// Generation metrics. A metric error is recorded per case rather
		// than aborting the run or silently leaving a zero score.
		if o.LLM != nil && tc.Response != "" {
			contextText := buildContextText(sr.Hits)

			faith, detail, err := Faithfulness(ctx, tc.Response, contextText, o.LLM)
			if err != nil {
				results[i].recordMetricError("faithfulness", err)
			} else {
				results[i].Faithfulness = faith
				results[i].FaithfulnessDetail = detail
			}

			if o.Embedders != nil {
				rel, err := AnswerRelevancy(ctx, tc.Query, tc.Response, o.LLM, o.Embedders, o.RelevancySampleCount)
				if err != nil {
					results[i].recordMetricError("answer_relevancy", err)
				} else {
					results[i].AnswerRelevancy = rel
				}
			}

			if tc.GroundTruth != "" {
				correctness, err := AnswerCorrectness(ctx, tc.Response, tc.GroundTruth, o.LLM)
				if err != nil {
					results[i].recordMetricError("answer_correctness", err)
				} else {
					results[i].AnswerCorrectness = correctness
				}
			}

			if o.JudgeRubric != "" {
				score, reason, err := LLMJudge(ctx, tc.Query, tc.Response, contextText, o.JudgeRubric, o.LLM)
				if err != nil {
					results[i].recordMetricError("llm_judge", err)
				} else {
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
