package eval

import (
	"context"
	"encoding/json"

	topeval "github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/rag/types"
)

// Annotation keys used by RAG subjects.
const (
	AnnotationHits          = "rag.hits"           // []types.SearchHit
	AnnotationRelevantUUIDs = "rag.relevant_uuids" // []string
	AnnotationContextText   = "rag.context_text"   // string

	// AnnotationRelevantKeys holds relevance labels of the kind named by
	// AnnotationRelevanceKey. When present it takes precedence over
	// AnnotationRelevantUUIDs.
	AnnotationRelevantKeys = "rag.relevant_keys" // []string
	AnnotationRelevanceKey = "rag.relevance_key" // RelevanceKey; default "variant"
)

// ContextPrecisionScorer wraps [ContextPrecision] as a [topeval.Scorer].
func ContextPrecisionScorer() topeval.Scorer {
	return topeval.NewScorerFunc("context_precision", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		return retrievalScore(obs, "context_precision", 0, func(s RetrievalScores) float64 { return s.ContextPrecision })
	})
}

// ContextRecallScorer wraps [ContextRecall] as a [topeval.Scorer].
func ContextRecallScorer() topeval.Scorer {
	return topeval.NewScorerFunc("context_recall", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		return retrievalScore(obs, "context_recall", 0, func(s RetrievalScores) float64 { return s.ContextRecall })
	})
}

// NDCGScorer wraps [NDCG] as a [topeval.Scorer].
func NDCGScorer(k int) topeval.Scorer {
	return topeval.NewScorerFunc("ndcg", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		return retrievalScore(obs, "ndcg", k, func(s RetrievalScores) float64 { return s.NDCG })
	})
}

// MRRScorer wraps [MRR] as a [topeval.Scorer].
func MRRScorer() topeval.Scorer {
	return topeval.NewScorerFunc("mrr", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		return retrievalScore(obs, "mrr", 0, func(s RetrievalScores) float64 { return s.MRR })
	})
}

// HitRateScorer wraps [HitRate] as a [topeval.Scorer].
func HitRateScorer(k int) topeval.Scorer {
	return topeval.NewScorerFunc("hit_rate", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		return retrievalScore(obs, "hit_rate", k, func(s RetrievalScores) float64 { return s.HitRate })
	})
}

// FaithfulnessScorer wraps [Faithfulness] as a [topeval.Scorer].
// It reads the response from Output and context from the AnnotationContextText annotation.
func FaithfulnessScorer(llm types.LLM) topeval.Scorer {
	return topeval.NewScorerFunc("faithfulness", func(ctx context.Context, obs topeval.Observation) (topeval.Score, error) {
		response, contextText, err := extractResponseAndContext(obs)
		if err != nil || response == "" {
			return topeval.Score{}, err
		}
		score, _, err := Faithfulness(ctx, response, contextText, llm)
		if err != nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "faithfulness", Value: score}, nil
	})
}

// AnswerRelevancyScorer wraps [AnswerRelevancy] as a [topeval.Scorer].
func AnswerRelevancyScorer(llm types.LLM, embedders types.EmbedderRegistry, sampleCount int) topeval.Scorer {
	return topeval.NewScorerFunc("answer_relevancy", func(ctx context.Context, obs topeval.Observation) (topeval.Score, error) {
		var query, response string
		if obs.Input != nil {
			if err := json.Unmarshal(obs.Input, &query); err != nil {
				return topeval.Score{}, err
			}
		}
		if obs.Output != nil {
			if err := json.Unmarshal(obs.Output, &response); err != nil {
				return topeval.Score{}, err
			}
		}
		if response == "" {
			return topeval.Score{}, nil
		}
		score, err := AnswerRelevancy(ctx, query, response, llm, embedders, sampleCount)
		if err != nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "answer_relevancy", Value: score}, nil
	})
}

// AnswerCorrectnessScorer wraps [AnswerCorrectness] as a [topeval.Scorer].
func AnswerCorrectnessScorer(llm types.LLM) topeval.Scorer {
	return topeval.NewScorerFunc("answer_correctness", func(ctx context.Context, obs topeval.Observation) (topeval.Score, error) {
		var response, groundTruth string
		if obs.Output != nil {
			if err := json.Unmarshal(obs.Output, &response); err != nil {
				return topeval.Score{}, err
			}
		}
		if obs.GroundTruth != nil {
			if err := json.Unmarshal(obs.GroundTruth, &groundTruth); err != nil {
				return topeval.Score{}, err
			}
		}
		if response == "" {
			return topeval.Score{}, nil
		}
		score, err := AnswerCorrectness(ctx, response, groundTruth, llm)
		if err != nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "answer_correctness", Value: score}, nil
	})
}

// retrievalScore computes one retrieval metric from obs's annotations. It
// returns an empty Score, which carries no signal, when the observation has
// no hits annotation or no relevance labels.
func retrievalScore(obs topeval.Observation, name string, k int, pick func(RetrievalScores) float64) (topeval.Score, error) {
	hits, labels, key, err := extractRAGAnnotations(obs)
	if err != nil {
		return topeval.Score{}, err
	}
	if hits == nil {
		return topeval.Score{}, nil
	}
	scores, ok := ScoreRetrieval(hits, labels, key, k)
	if !ok {
		return topeval.Score{}, nil
	}
	return topeval.Score{Name: name, Value: pick(scores)}, nil
}

func extractRAGAnnotations(obs topeval.Observation) ([]types.SearchHit, []string, RelevanceKey, error) {
	var hits []types.SearchHit
	if raw, ok := obs.Annotations[AnnotationHits]; ok {
		if err := json.Unmarshal(raw, &hits); err != nil {
			return nil, nil, "", err
		}
	}
	if raw, ok := obs.Annotations[AnnotationRelevantKeys]; ok {
		var keys []string
		if err := json.Unmarshal(raw, &keys); err != nil {
			return nil, nil, "", err
		}
		key := RelevanceVariant
		if rawKey, ok := obs.Annotations[AnnotationRelevanceKey]; ok {
			if err := json.Unmarshal(rawKey, &key); err != nil {
				return nil, nil, "", err
			}
			if err := key.Validate(); err != nil {
				return nil, nil, "", err
			}
		}
		if len(keys) > 0 {
			return hits, keys, key, nil
		}
	}
	var uuids []string
	if raw, ok := obs.Annotations[AnnotationRelevantUUIDs]; ok {
		if err := json.Unmarshal(raw, &uuids); err != nil {
			return nil, nil, "", err
		}
	}
	return hits, uuids, RelevanceVariant, nil
}

func extractResponseAndContext(obs topeval.Observation) (string, string, error) {
	var response string
	if obs.Output != nil {
		if err := json.Unmarshal(obs.Output, &response); err != nil {
			return "", "", err
		}
	}
	var contextText string
	if raw, ok := obs.Annotations[AnnotationContextText]; ok {
		if err := json.Unmarshal(raw, &contextText); err != nil {
			return "", "", err
		}
	}
	return response, contextText, nil
}
