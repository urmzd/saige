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
// Matching is case-insensitive on (Name, Type) pairs.
func EntityRecallScorer() topeval.Scorer {
	return topeval.NewScorerFunc("entity_recall", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractEntityPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		if len(expected) == 0 {
			return topeval.Score{Name: "entity_recall", Value: 1.0}, nil
		}

		extractedSet := make(map[string]struct{}, len(extracted))
		for _, e := range extracted {
			extractedSet[entityKey(e.Name, e.Type)] = struct{}{}
		}

		var found int
		for _, e := range expected {
			if _, ok := extractedSet[entityKey(e.Name, e.Type)]; ok {
				found++
			}
		}
		return topeval.Score{Name: "entity_recall", Value: float64(found) / float64(len(expected))}, nil
	})
}

// EntityPrecisionScorer computes the fraction of extracted entities that match expected.
func EntityPrecisionScorer() topeval.Scorer {
	return topeval.NewScorerFunc("entity_precision", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractEntityPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		if len(extracted) == 0 {
			return topeval.Score{Name: "entity_precision", Value: 1.0}, nil
		}

		expectedSet := make(map[string]struct{}, len(expected))
		for _, e := range expected {
			expectedSet[entityKey(e.Name, e.Type)] = struct{}{}
		}

		var found int
		for _, e := range extracted {
			if _, ok := expectedSet[entityKey(e.Name, e.Type)]; ok {
				found++
			}
		}
		return topeval.Score{Name: "entity_precision", Value: float64(found) / float64(len(extracted))}, nil
	})
}

// RelationRecallScorer computes the fraction of expected relations that were extracted.
// Matching is case-insensitive on (Source, Target, Type).
func RelationRecallScorer() topeval.Scorer {
	return topeval.NewScorerFunc("relation_recall", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractRelationPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		if len(expected) == 0 {
			return topeval.Score{Name: "relation_recall", Value: 1.0}, nil
		}

		extractedSet := make(map[string]struct{}, len(extracted))
		for _, r := range extracted {
			extractedSet[relationKey(r.Source, r.Target, r.Type)] = struct{}{}
		}

		var found int
		for _, r := range expected {
			if _, ok := extractedSet[relationKey(r.Source, r.Target, r.Type)]; ok {
				found++
			}
		}
		return topeval.Score{Name: "relation_recall", Value: float64(found) / float64(len(expected))}, nil
	})
}

// RelationPrecisionScorer computes the fraction of extracted relations that match expected.
func RelationPrecisionScorer() topeval.Scorer {
	return topeval.NewScorerFunc("relation_precision", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		extracted, expected, err := extractRelationPair(obs)
		if err != nil || expected == nil {
			return topeval.Score{}, err
		}
		if len(extracted) == 0 {
			return topeval.Score{Name: "relation_precision", Value: 1.0}, nil
		}

		expectedSet := make(map[string]struct{}, len(expected))
		for _, r := range expected {
			expectedSet[relationKey(r.Source, r.Target, r.Type)] = struct{}{}
		}

		var found int
		for _, r := range extracted {
			if _, ok := expectedSet[relationKey(r.Source, r.Target, r.Type)]; ok {
				found++
			}
		}
		return topeval.Score{Name: "relation_precision", Value: float64(found) / float64(len(extracted))}, nil
	})
}

// FactSearchRecallScorer computes the fraction of relevant fact UUIDs found by search.
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
		if len(relevantUUIDs) == 0 {
			return topeval.Score{Name: "fact_search_recall", Value: 1.0}, nil
		}

		foundSet := make(map[string]struct{}, len(facts))
		for _, f := range facts {
			foundSet[f.UUID] = struct{}{}
		}

		var found int
		for _, uuid := range relevantUUIDs {
			if _, ok := foundSet[uuid]; ok {
				found++
			}
		}
		return topeval.Score{Name: "fact_search_recall", Value: float64(found) / float64(len(relevantUUIDs))}, nil
	})
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
