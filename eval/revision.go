package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// RevisionTag returns a content hash of JSON-serializable configuration.
// It panics for non-serializable values; pass declarative configuration,
// never provider objects, functions, or credentials.
func RevisionTag(values ...any) string {
	raw, err := json.Marshal(values)
	if err != nil {
		panic(fmt.Sprintf("eval revision: %v", err))
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Revisioned pins a scorer's implementation and configuration revision.
// Use this for custom closures whose captured configuration cannot be inferred.
// Empty revisions panic rather than silently losing provenance.
func Revisioned(s Scorer, revision string) Scorer {
	if revision == "" {
		panic("eval: empty scorer revision")
	}
	return &revisionScorer{Scorer: s, revision: revision}
}

type revisionScorer struct {
	Scorer
	revision      string
	configuration json.RawMessage
}

func (s *revisionScorer) Revision() string { return s.revision }
func (s *revisionScorer) Deterministic() bool {
	d, ok := s.Scorer.(deterministic)
	return ok && d.Deterministic()
}
func (s *revisionScorer) Score(ctx context.Context, obs Observation) (Score, error) {
	score, err := s.Scorer.Score(ctx, obs)
	if score.Name != "" {
		score.Revision = s.revision
		if score.Samples != nil {
			stats := *score.Samples
			stats.Revision = s.revision
			score.Samples = &stats
		}
	}
	return score, err
}

// ScorerRevision reports an explicit revision when available, otherwise an
// implementation identity tag. Identity tags cannot discover closure state:
// pin custom configuration with Revisioned. Registry scorers and judges
// include their declarative configuration automatically.
func ScorerRevision(s Scorer) string {
	if r, ok := s.(interface{ Revision() string }); ok && r.Revision() != "" {
		return r.Revision()
	}
	return RevisionTag("scorer/v1", fmt.Sprintf("%T", s), s.Name())
}

func stampScore(s Scorer, score Score) Score {
	if score.Name != "" && score.Revision == "" {
		score.Revision = ScorerRevision(s)
	}
	return score
}

// Raw payloads are hashed as strings so malformed JSON remains a scoreable
// case, rather than panicking while its revision is computed.
func observationRevision(obs Observation) string {
	annotations := make(map[string]string, len(obs.Annotations))
	for k, v := range obs.Annotations {
		annotations[k] = string(v)
	}
	return RevisionTag("observation/v1", obs.ID, obs.Turn, obs.Sample, obs.Labels,
		string(obs.Input), string(obs.Output), string(obs.GroundTruth), annotations)
}

// ScorerConfiguration returns declared settings when a scorer exposes them.
// Opaque closures require Config on the experiment or variant and a revision pin.
func ScorerConfiguration(s Scorer) json.RawMessage {
	if c, ok := s.(interface{ Configuration() json.RawMessage }); ok {
		return append(json.RawMessage(nil), c.Configuration()...)
	}
	return nil
}
func (s *revisionScorer) Configuration() json.RawMessage {
	if len(s.configuration) > 0 {
		return append(json.RawMessage(nil), s.configuration...)
	}
	return ScorerConfiguration(s.Scorer)
}
func withScorerConfiguration(s Scorer, config any) Scorer {
	raw, err := json.Marshal(config)
	if err != nil {
		panic(fmt.Sprintf("eval scorer configuration: %v", err))
	}
	return &revisionScorer{Scorer: s, revision: ScorerRevision(s), configuration: raw}
}
