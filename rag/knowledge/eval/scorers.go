// Package eval provides knowledge-graph-specific evaluation scorers.
package eval

import (
	"context"
	"encoding/json"
	"strings"

	topeval "github.com/urmzd/saige/eval"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

// Annotation keys used by KG subjects.
const (
	AnnotationExtractedEntities  = "kg.extracted_entities"  // []kgtypes.ExtractedEntity
	AnnotationExtractedRelations = "kg.extracted_relations" // []kgtypes.ExtractedRelation
	AnnotationExpectedEntities   = "kg.expected_entities"   // []kgtypes.ExtractedEntity
	AnnotationExpectedRelations  = "kg.expected_relations"  // []kgtypes.ExtractedRelation
	AnnotationSearchedFacts      = "kg.searched_facts"      // []kgtypes.Fact
	AnnotationRelevantFacts      = "kg.relevant_facts"      // []string (UUIDs)
)

// EntityRecallScorer computes the fraction of expected entities that were extracted.
// Matching is case-insensitive on (Name, Type) pairs, and both sides are
// deduplicated by that key first, so a repeated entity counts once. With no
// expected entities the score has no signal and Score{} is returned.
func EntityRecallScorer() topeval.Scorer {
	return topeval.NewScorerFunc("entity_recall", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractEntityPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		return setScore("entity_recall", entityKeys(expected), entityKeys(extracted)), nil
	})
}

// EntityPrecisionScorer computes the fraction of extracted entities that match expected.
// Keys are deduplicated as in EntityRecallScorer. With no extracted entities
// the score has no signal and Score{} is returned.
func EntityPrecisionScorer() topeval.Scorer {
	return topeval.NewScorerFunc("entity_precision", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractEntityPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		return setScore("entity_precision", entityKeys(extracted), entityKeys(expected)), nil
	})
}

// RelationRecallScorer computes the fraction of expected relations that were extracted.
// Matching is case-insensitive on (Source, Target, Type), with both sides
// deduplicated by that key. With no expected relations Score{} is returned.
func RelationRecallScorer() topeval.Scorer {
	return topeval.NewScorerFunc("relation_recall", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractRelationPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		return setScore("relation_recall", relationKeys(expected), relationKeys(extracted)), nil
	})
}

// RelationPrecisionScorer computes the fraction of extracted relations that match expected.
// Keys are deduplicated as in RelationRecallScorer. With no extracted
// relations Score{} is returned.
func RelationPrecisionScorer() topeval.Scorer {
	return topeval.NewScorerFunc("relation_precision", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractRelationPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		return setScore("relation_precision", relationKeys(extracted), relationKeys(expected)), nil
	})
}

// FactSearchRecallScorer computes the fraction of relevant fact UUIDs found by search.
// Relevant UUIDs are deduplicated. With no relevant UUIDs Score{} is returned.
func FactSearchRecallScorer() topeval.Scorer {
	return topeval.NewScorerFunc("fact_search_recall", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		var facts []kgtypes.Fact
		var relevantUUIDs []string

		if raw, ok := obs.Annotations[AnnotationSearchedFacts]; ok {
			if err := json.Unmarshal(raw, &facts); err != nil {
				return topeval.Score{}, err
			}
		} else {
			return topeval.Score{}, nil
		}

		if raw, ok := obs.Annotations[AnnotationRelevantFacts]; ok {
			if err := json.Unmarshal(raw, &relevantUUIDs); err != nil {
				return topeval.Score{}, err
			}
		}

		found := make(map[string]struct{}, len(facts))
		for _, f := range facts {
			found[f.UUID] = struct{}{}
		}
		relevant := make(map[string]struct{}, len(relevantUUIDs))
		for _, id := range relevantUUIDs {
			relevant[id] = struct{}{}
		}
		return setScore("fact_search_recall", relevant, found), nil
	})
}

// setScore returns |denominator ∩ other| / |denominator| under name, or the
// no-signal Score{} when denominator is empty.
func setScore(name string, denominator, other map[string]struct{}) topeval.Score {
	if len(denominator) == 0 {
		return topeval.Score{}
	}
	var hits int
	for k := range denominator {
		if _, ok := other[k]; ok {
			hits++
		}
	}
	return topeval.Score{Name: name, Value: float64(hits) / float64(len(denominator))}
}

func entityKeys(ents []kgtypes.ExtractedEntity) map[string]struct{} {
	out := make(map[string]struct{}, len(ents))
	for _, e := range ents {
		out[entityKey(e.Name, e.Type)] = struct{}{}
	}
	return out
}

func relationKeys(rels []kgtypes.ExtractedRelation) map[string]struct{} {
	out := make(map[string]struct{}, len(rels))
	for _, r := range rels {
		out[relationKey(r.Source, r.Target, r.Type)] = struct{}{}
	}
	return out
}

func entityKey(name, typ string) string {
	return strings.ToLower(name) + "|" + strings.ToLower(typ)
}

func relationKey(source, target, typ string) string {
	return strings.ToLower(source) + "|" + strings.ToLower(target) + "|" + strings.ToLower(typ)
}

func extractEntityPair(obs topeval.Observation) ([]kgtypes.ExtractedEntity, []kgtypes.ExtractedEntity, error) {
	var extracted, expected []kgtypes.ExtractedEntity
	if raw, ok := obs.Annotations[AnnotationExtractedEntities]; ok {
		if err := json.Unmarshal(raw, &extracted); err != nil {
			return nil, nil, err
		}
	}
	if raw, ok := obs.Annotations[AnnotationExpectedEntities]; ok {
		if err := json.Unmarshal(raw, &expected); err != nil {
			return nil, nil, err
		}
	}
	return extracted, expected, nil
}

func extractRelationPair(obs topeval.Observation) ([]kgtypes.ExtractedRelation, []kgtypes.ExtractedRelation, error) {
	var extracted, expected []kgtypes.ExtractedRelation
	if raw, ok := obs.Annotations[AnnotationExtractedRelations]; ok {
		if err := json.Unmarshal(raw, &extracted); err != nil {
			return nil, nil, err
		}
	}
	if raw, ok := obs.Annotations[AnnotationExpectedRelations]; ok {
		if err := json.Unmarshal(raw, &expected); err != nil {
			return nil, nil, err
		}
	}
	return extracted, expected, nil
}
