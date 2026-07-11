package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/urmzd/saige/knowledge/types"
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
		relUUID, srcID, tgtID, rel.Type, rel.Fact, validAt, rel.GroupID,
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

// FindRelationsBetweenEntities returns all relations between two entities (bidirectional).
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

// getNeighbors returns immediate neighbors and edges for an entity UUID.
func (s *Store) getNeighbors(ctx context.Context, nodeUUID string) ([]types.GraphNode, []types.GraphEdge, error) {
	nodeID, err := s.entityID(ctx, nodeUUID)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.pool.Query(ctx, relationNeighborsSQL, nodeID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var neighbors []types.GraphNode
	var edges []types.GraphEdge
	seen := make(map[string]bool)

	for rows.Next() {
		var (
			rUUID, rType, rFact        string
			rCreatedAt, rValidAt       time.Time
			rInvalidAt                 *time.Time
			nUUID, nName, nType, nSumm string
			isOutgoing                 bool
		)
		if err := rows.Scan(&rUUID, &rType, &rFact, &rCreatedAt, &rValidAt, &rInvalidAt,
			&nUUID, &nName, &nType, &nSumm, &isOutgoing); err != nil {
			return nil, nil, err
		}

		if !seen[nUUID] {
			seen[nUUID] = true
			neighbors = append(neighbors, types.GraphNode{
				ID: nUUID, Name: nName, Type: nType, Summary: nSumm,
			})
		}

		src, tgt := nUUID, nodeUUID
		if isOutgoing {
			src, tgt = nodeUUID, nUUID
		}
		edges = append(edges, types.GraphEdge{
			ID: rUUID, Source: src, Target: tgt,
			Type: rType, Fact: rFact, Weight: 1.0,
			CreatedAt: rCreatedAt, ValidAt: rValidAt, InvalidAt: rInvalidAt,
		})
	}

	return neighbors, edges, rows.Err()
}
