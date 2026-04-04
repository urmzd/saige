package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/agent/types"
)

// pgStoreTx implements types.StoreTx within a PostgreSQL transaction.
type pgStoreTx struct {
	tx pgx.Tx
}

func (t *pgStoreTx) SaveNode(ctx context.Context, node *types.Node) error {
	return saveNode(ctx, t.tx, node)
}

func (t *pgStoreTx) SaveBranch(ctx context.Context, branch types.BranchID, tipID types.NodeID) error {
	return saveBranch(ctx, t.tx, branch, tipID)
}

func (t *pgStoreTx) SaveCheckpoint(ctx context.Context, cp types.Checkpoint) error {
	return saveCheckpoint(ctx, t.tx, cp)
}

// Tx executes fn within a PostgreSQL transaction.
func (s *Store) Tx(ctx context.Context, fn func(types.StoreTx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := fn(&pgStoreTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
