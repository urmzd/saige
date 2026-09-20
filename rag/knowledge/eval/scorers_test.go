package eval

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	topeval "github.com/urmzd/saige/eval"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

func assertClose(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s: got %f, want %f (±%f)", name, got, want, eps)
	}
}

func makeEntityObs(extracted, expected []kgtypes.ExtractedEntity) topeval.Observation {
	ann := map[string]json.RawMessage{}
	if extracted != nil {
		b, _ := json.Marshal(extracted)
		ann[AnnotationExtractedEntities] = b
	}
	if expected != nil {
		b, _ := json.Marshal(expected)
		ann[AnnotationExpectedEntities] = b
	}
	return topeval.Observation{ID: "kg-test", Annotations: ann}
}

func TestEntityRecall(t *testing.T) {
	extracted := []kgtypes.ExtractedEntity{
		{Name: "Alice", Type: "Person"},
		{Name: "Bob", Type: "Person"},
	}
	expected := []kgtypes.ExtractedEntity{
		{Name: "alice", Type: "person"}, // case-insensitive match
		{Name: "Charlie", Type: "Person"},
	}

	obs := makeEntityObs(extracted, expected)
	score, err := EntityRecallScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	// Found alice, missed charlie → 1/2 = 0.5
	assertClose(t, "entity_recall", score.Value, 0.5, 0.001)
}

func TestEntityPrecision(t *testing.T) {
	extracted := []kgtypes.ExtractedEntity{
		{Name: "Alice", Type: "Person"},
		{Name: "Bob", Type: "Person"},
		{Name: "Unknown", Type: "Org"},
	}
	expected := []kgtypes.ExtractedEntity{
		{Name: "Alice", Type: "Person"},
		{Name: "Bob", Type: "Person"},
	}

	obs := makeEntityObs(extracted, expected)
	score, err := EntityPrecisionScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	// 2 out of 3 extracted match expected → 2/3
	assertClose(t, "entity_precision", score.Value, 2.0/3.0, 0.001)
}

func TestRelationRecall(t *testing.T) {
	extracted := []kgtypes.ExtractedRelation{
		{Source: "Alice", Target: "Bob", Type: "knows"},
	}
	expected := []kgtypes.ExtractedRelation{
		{Source: "alice", Target: "bob", Type: "knows"},
		{Source: "Bob", Target: "Charlie", Type: "works_with"},
	}

	ann := map[string]json.RawMessage{}
	b, _ := json.Marshal(extracted)
	ann[AnnotationExtractedRelations] = b
	b, _ = json.Marshal(expected)
	ann[AnnotationExpectedRelations] = b

	obs := topeval.Observation{ID: "r1", Annotations: ann}
	score, err := RelationRecallScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	// Found 1 of 2 → 0.5
	assertClose(t, "relation_recall", score.Value, 0.5, 0.001)
}

func TestEntityRecallMissingAnnotation(t *testing.T) {
	obs := topeval.Observation{ID: "empty"}
	score, err := EntityRecallScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	if score.Name != "" {
		t.Errorf("expected empty score for missing annotations, got %q", score.Name)
	}
}

func TestFactSearchRecall(t *testing.T) {
	facts := []kgtypes.Fact{
		{UUID: "f1"},
		{UUID: "f2"},
		{UUID: "f3"},
	}
	relevant := []string{"f1", "f3", "f4"}

	ann := map[string]json.RawMessage{}
	b, _ := json.Marshal(facts)
	ann[AnnotationSearchedFacts] = b
	b, _ = json.Marshal(relevant)
	ann[AnnotationRelevantFacts] = b

	obs := topeval.Observation{ID: "fs1", Annotations: ann}
	score, err := FactSearchRecallScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	// Found f1, f3 out of f1, f3, f4 → 2/3
	assertClose(t, "fact_recall", score.Value, 2.0/3.0, 0.001)
}
