package pgstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/knowledge/types"
)

// UpsertEntity creates or updates an entity by (group_id, name, type), returning its UUID.
func (s *Store) UpsertEntity(ctx context.Context, entity *types.ExtractedEntity, embedding []float32) (string, error) {
	entUUID := uuid.New().String()

	var emb *pgvector.Vector
	if embedding != nil {
		v := pgvector.NewVector(embedding)
		emb = &v
	}

	var resultUUID string
	err := s.pool.QueryRow(ctx, entityUpsertSQL,
		entUUID, entity.Name, entity.Type, entity.Summary, emb, entity.GroupID,
	).Scan(&resultUUID)
	if err != nil {
		return "", fmt.Errorf("upsert entity %s: %w", entity.Name, err)
	}

	return resultUUID, nil
}

// GetEntity retrieves an entity by UUID.
func (s *Store) GetEntity(ctx context.Context, id string) (*types.Entity, error) {
	var e types.Entity
	err := s.pool.QueryRow(ctx, entityGetSQL, id).Scan(&e.UUID, &e.Name, &e.Type, &e.Summary)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("%w: %s", types.ErrNodeNotFound, id)
		}
		return nil, err
	}
	return &e, nil
}

// FindEntitiesByNameType finds entities with exact name+type match.
func (s *Store) FindEntitiesByNameType(ctx context.Context, name, entityType string) ([]types.Entity, error) {
	rows, err := s.pool.Query(ctx, entityFindByNameTypeSQL, name, entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEntities(rows)
}

// FindEntitiesByFuzzyName returns entities whose names approximately match, using trigram similarity.
func (s *Store) FindEntitiesByFuzzyName(ctx context.Context, name string, limit int) ([]types.Entity, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, entityFindFuzzySQL, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEntities(rows)
}

// FindEntitiesByNameTypeInGroup finds entities with exact name+type match within a group.
func (s *Store) FindEntitiesByNameTypeInGroup(ctx context.Context, groupID, name, entityType string) ([]types.Entity, error) {
	rows, err := s.pool.Query(ctx, entityFindByNameTypeGroupSQL, groupID, name, entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEntities(rows)
}

// FindEntitiesByFuzzyNameInGroup returns entities within a group whose names
// approximately match, using trigram similarity.
func (s *Store) FindEntitiesByFuzzyNameInGroup(ctx context.Context, groupID, name string, limit int) ([]types.Entity, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, entityFindFuzzyGroupSQL, groupID, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEntities(rows)
}

// entityID resolves an entity UUID to its internal BIGSERIAL id.
func (s *Store) entityID(ctx context.Context, entityUUID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, entityIDSQL, entityUUID).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("%w: %s", types.ErrNodeNotFound, entityUUID)
		}
		return 0, err
	}
	return id, nil
}

func scanEntities(rows pgx.Rows) ([]types.Entity, error) {
	var entities []types.Entity
	for rows.Next() {
		var e types.Entity
		if err := rows.Scan(&e.UUID, &e.Name, &e.Type, &e.Summary); err != nil {
			return nil, err
		}
		entities = append(entities, e)
	}
	return entities, rows.Err()
}
