package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/agent/types"
)

func saveCheckpoint(ctx context.Context, q querier, cp types.Checkpoint) error {
	_, err := q.Exec(ctx, checkpointUpsertSQL,
		string(cp.ID),
		string(cp.Branch),
		string(cp.NodeID),
		cp.Name,
		cp.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert checkpoint %s: %w", cp.ID, err)
	}
	return nil
}

// SaveCheckpoint persists a checkpoint.
func (s *Store) SaveCheckpoint(ctx context.Context, cp types.Checkpoint) error {
	return saveCheckpoint(ctx, s.pool, cp)
}

// LoadCheckpoint retrieves a checkpoint by ID.
func (s *Store) LoadCheckpoint(ctx context.Context, id types.CheckpointID) (types.Checkpoint, error) {
	var (
		cp types.Checkpoint
	)
	var uuid, branchID, nodeUUID, name string
	err := s.pool.QueryRow(ctx, checkpointGetSQL, string(id)).Scan(
		&uuid, &branchID, &nodeUUID, &name, &cp.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return cp, fmt.Errorf("checkpoint not found: %s", id)
		}
		return cp, err
	}
	cp.ID = types.CheckpointID(uuid)
	cp.Branch = types.BranchID(branchID)
	cp.NodeID = types.NodeID(nodeUUID)
	cp.Name = name
	return cp, nil
}
