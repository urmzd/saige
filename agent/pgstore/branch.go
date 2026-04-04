package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/agent/types"
)

func saveBranch(ctx context.Context, q querier, branch types.BranchID, tipID types.NodeID) error {
	_, err := q.Exec(ctx, branchUpsertSQL, string(branch), string(tipID))
	if err != nil {
		return fmt.Errorf("upsert branch %s: %w", branch, err)
	}
	return nil
}

// SaveBranch persists a branch-to-tip mapping.
func (s *Store) SaveBranch(ctx context.Context, branch types.BranchID, tipID types.NodeID) error {
	return saveBranch(ctx, s.pool, branch, tipID)
}

// LoadBranch retrieves the tip node ID for a branch.
func (s *Store) LoadBranch(ctx context.Context, branch types.BranchID) (types.NodeID, error) {
	var tip string
	err := s.pool.QueryRow(ctx, branchGetSQL, string(branch)).Scan(&tip)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("branch not found: %s", branch)
		}
		return "", err
	}
	return types.NodeID(tip), nil
}

// ListBranches returns all branch-to-tip mappings.
func (s *Store) ListBranches(ctx context.Context) (map[types.BranchID]types.NodeID, error) {
	rows, err := s.pool.Query(ctx, branchListSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	branches := make(map[types.BranchID]types.NodeID)
	for rows.Next() {
		var bid, tip string
		if err := rows.Scan(&bid, &tip); err != nil {
			return nil, err
		}
		branches[types.BranchID(bid)] = types.NodeID(tip)
	}
	return branches, rows.Err()
}
