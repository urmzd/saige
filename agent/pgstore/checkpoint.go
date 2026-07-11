package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/agent/types"
)

func saveCheckpoint(ctx context.Context, q querier, conversationID string, cp types.Checkpoint) error {
	_, err := q.Exec(ctx, checkpointUpsertSQL,
		string(cp.ID),
		conversationID,
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

// SaveCheckpoint persists a checkpoint within this store's conversation.
func (s *Store) SaveCheckpoint(ctx context.Context, cp types.Checkpoint) error {
	return saveCheckpoint(ctx, s.pool, s.conversationID, cp)
}

// LoadCheckpoint retrieves a checkpoint by ID within this store's conversation.
func (s *Store) LoadCheckpoint(ctx context.Context, id types.CheckpointID) (types.Checkpoint, error) {
	var (
		cp types.Checkpoint
	)
	var uuid, branchID, nodeUUID, name string
	err := s.pool.QueryRow(ctx, checkpointGetSQL, s.conversationID, string(id)).Scan(
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

// ListCheckpoints returns all checkpoints within this store's conversation,
// ordered by creation time.
func (s *Store) ListCheckpoints(ctx context.Context) ([]types.Checkpoint, error) {
	rows, err := s.pool.Query(ctx, checkpointListSQL, s.conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checkpoints []types.Checkpoint
	for rows.Next() {
		var cp types.Checkpoint
		var uuid, branchID, nodeUUID, name string
		if err := rows.Scan(&uuid, &branchID, &nodeUUID, &name, &cp.CreatedAt); err != nil {
			return nil, err
		}
		cp.ID = types.CheckpointID(uuid)
		cp.Branch = types.BranchID(branchID)
		cp.NodeID = types.NodeID(nodeUUID)
		cp.Name = name
		checkpoints = append(checkpoints, cp)
	}
	return checkpoints, rows.Err()
}
