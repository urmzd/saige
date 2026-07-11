// Package walrecover replays committed-but-unapplied WAL transactions into a
// types.Store. Call RecoverWAL at startup, before loading the tree from the
// store, so a crash between WAL commit and store write is healed and the
// loaded tree reflects every committed mutation.
package walrecover

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// applier is optionally implemented by WALs (agent/store/filewal,
// agent/store/memwal) that can mark a transaction as applied so it is not
// returned by future Recover calls.
type applier interface {
	MarkApplied(ctx context.Context, txID types.TxID) error
}

// RecoverWAL replays every committed-but-unapplied WAL transaction into store,
// each inside one store transaction, and returns the number of WAL
// transactions applied. Ops map to store writes by kind: node ops call
// SaveNode, branch ops call SaveBranch, checkpoint ops call SaveCheckpoint —
// all idempotent upserts, so re-running recovery is safe. When the WAL
// supports it, applied transactions are marked so they are skipped next time.
func RecoverWAL(ctx context.Context, wal types.WAL, store types.Store) (int, error) {
	txIDs, err := wal.Recover(ctx)
	if err != nil {
		return 0, fmt.Errorf("walrecover: recover: %w", err)
	}

	marker, _ := wal.(applier)
	applied := 0
	for _, txID := range txIDs {
		ops, err := wal.Replay(ctx, txID)
		if err != nil {
			return applied, fmt.Errorf("walrecover: replay %s: %w", txID, err)
		}
		if err := store.Tx(ctx, func(tx types.StoreTx) error {
			return applyOps(ctx, tx, ops)
		}); err != nil {
			return applied, fmt.Errorf("walrecover: apply %s: %w", txID, err)
		}
		if marker != nil {
			if err := marker.MarkApplied(ctx, txID); err != nil {
				return applied, fmt.Errorf("walrecover: mark applied %s: %w", txID, err)
			}
		}
		applied++
	}
	return applied, nil
}

func applyOps(ctx context.Context, tx types.StoreTx, ops []types.TxOp) error {
	for _, op := range ops {
		switch op.Kind {
		case types.TxOpAddNode, types.TxOpUpdateNode, types.TxOpAddChild:
			if op.Node == nil {
				return fmt.Errorf("op %s has no node", op.Kind)
			}
			if err := tx.SaveNode(ctx, op.Node); err != nil {
				return err
			}
		case types.TxOpSetBranch:
			if err := tx.SaveBranch(ctx, op.BranchID, op.TipID); err != nil {
				return err
			}
		case types.TxOpAddCheckpoint:
			if op.Checkpoint == nil {
				return fmt.Errorf("op %s has no checkpoint", op.Kind)
			}
			if err := tx.SaveCheckpoint(ctx, *op.Checkpoint); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown op kind %q", op.Kind)
		}
	}
	return nil
}
