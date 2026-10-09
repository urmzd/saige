package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/urmzd/saige/rag/knowledge/types"
)

// CreateRelation creates a relation edge between two entities, returning the relation UUID.
func (s *Store) CreateRelation(ctx context.Context, rel *types.RelationInput) (string, error) {
	srcID, err := s.entityID(ctx, rel.SourceUUID)
	if err != nil {
		return "", fmt.Errorf("source entity %s: %w", rel.SourceUUID, err)
	}
	tgtID, err := s.entityID(ctx, rel.TargetUUID)
	if err != nil {
		return "", fmt.Errorf("target entity %s: %w", rel.TargetUUID, err)
	}

	relUUID := uuid.New().String()
	validAt := rel.ValidAt
	if validAt.IsZero() {
		validAt = time.Now()
	}

	_, err = s.pool.Exec(ctx, relationCreateSQL,
		relUUID, srcID, tgtID, rel.Type, rel.Fact, validAt, rel.InvalidAt, rel.GroupID,
	)
	if err != nil {
		return "", fmt.Errorf("create relation: %w", err)
	}

	return relUUID, nil
}

// InvalidateRelation marks a relation as no longer valid.
func (s *Store) InvalidateRelation(ctx context.Context, relUUID string, invalidAt time.Time) error {
	_, err := s.pool.Exec(ctx, relationInvalidateSQL, invalidAt, relUUID)
	if err != nil {
		return fmt.Errorf("invalidate relation %s: %w", relUUID, err)
	}
	return nil
}

// FindRelationsBetweenEntities returns all relations between two entities in
// either direction, oldest first. Callers that care about direction compare
// SourceUUID.
func (s *Store) FindRelationsBetweenEntities(ctx context.Context, srcUUID, tgtUUID string) ([]types.Relation, error) {
	rows, err := s.pool.Query(ctx, relationFindBetweenSQL, srcUUID, tgtUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rels []types.Relation
	for rows.Next() {
		var r types.Relation
		if err := rows.Scan(&r.UUID, &r.Type, &r.Fact, &r.CreatedAt, &r.ValidAt, &r.InvalidAt,
			&r.SourceUUID, &r.TargetUUID); err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	return rels, rows.Err()
}

// Close is a no-op; the pool is externally managed.
func (s *Store) Close(_ context.Context) error {
	return nil
}
