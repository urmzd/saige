// Package walrecover replays committed-but-unapplied WAL transactions into a
// types.Store. Call RecoverWAL at startup, before loading the tree from the
// store, so a crash between WAL commit and store write is healed and the
// loaded tree reflects every committed mutation.
package walrecover

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// Transaction is one committed WAL transaction and its ops.
type Transaction struct {
	ID  types.TxID
	Ops []types.TxOp
}

// OpsRecoverer is optionally implemented by WALs (agent/store/filewal) that
// can return every pending transaction with its ops in one pass. Without it,
// RecoverWAL calls Replay once per transaction, which costs a full log read
// each time for a file-backed log.
type OpsRecoverer interface {
	RecoverOps(ctx context.Context) ([]Transaction, error)
}

// applier is optionally implemented by WALs (agent/store/filewal,
// agent/store/memwal) that can mark a transaction as applied so it is not
// returned by future Recover calls.
type applier interface {
	MarkApplied(ctx context.Context, txID types.TxID) error
}

// BatchApplier is optionally implemented by WALs (agent/store/filewal) that
// can mark many transactions applied with one durable write. RecoverWAL
// prefers it over per-transaction MarkApplied.
type BatchApplier interface {
	MarkAppliedBatch(ctx context.Context, txIDs []types.TxID) error
}

// Compactor is optionally implemented by WALs (agent/store/filewal) that can
// rewrite their log to drop applied transactions. RecoverWAL invokes it after
// a successful recovery pass so the log, which grows unboundedly during a
// session because normal-path writes never mark transactions applied, is
// shrunk back to only the transactions still awaiting application. Unrelated
// to agent/types.Compactor, which compacts message history.
type Compactor interface {
	Compact(ctx context.Context) error
}

// BatchSize is the number of WAL transactions RecoverWAL applies per store
// transaction. Every op is an idempotent upsert, so grouping transactions
// keeps commit order and only changes how many store round trips recovery
// pays.
const BatchSize = 256

// RecoverWAL replays every committed-but-unapplied WAL transaction into store
// and returns the number of WAL transactions applied. Transactions are applied
// in commit order, BatchSize at a time per store transaction, through
// tree.ApplyOps: node ops call SaveNode, branch ops call SaveBranch,
// checkpoint ops call SaveCheckpoint and active-branch ops call
// SaveActiveBranch when the store supports it. A node write the store rejects
// with tree.ErrVersionConflict is skipped, because the store already holds a
// newer version of that node. When the WAL supports it, applied transactions
// are marked so they are skipped next time, and the log is compacted
// afterwards so recovery leaves a minimal log.
func RecoverWAL(ctx context.Context, wal types.WAL, store types.Store) (int, error) {
	txs, err := pending(ctx, wal)
	if err != nil {
		return 0, err
	}

	applied := 0
	for start := 0; start < len(txs); start += BatchSize {
		batch := txs[start:min(start+BatchSize, len(txs))]
		if err := store.Tx(ctx, func(tx types.StoreTx) error {
			rtx := recoveryTx{tx}
			for _, t := range batch {
				if err := tree.ApplyOps(ctx, rtx, t.Ops); err != nil {
					return fmt.Errorf("%s: %w", t.ID, err)
				}
			}
			return nil
		}); err != nil {
			return applied, fmt.Errorf("walrecover: apply: %w", err)
		}
		if err := markApplied(ctx, wal, batch); err != nil {
			return applied, err
		}
		applied += len(batch)
	}
	if compactor, ok := wal.(Compactor); ok {
		if err := compactor.Compact(ctx); err != nil {
			return applied, fmt.Errorf("walrecover: compact: %w", err)
		}
	}
	return applied, nil
}

// pending returns the WAL's committed-but-unapplied transactions in commit
// order, in one pass when the WAL supports OpsRecoverer.
func pending(ctx context.Context, wal types.WAL) ([]Transaction, error) {
	if r, ok := wal.(OpsRecoverer); ok {
		txs, err := r.RecoverOps(ctx)
		if err != nil {
			return nil, fmt.Errorf("walrecover: recover: %w", err)
		}
		return txs, nil
	}
	txIDs, err := wal.Recover(ctx)
	if err != nil {
		return nil, fmt.Errorf("walrecover: recover: %w", err)
	}
	txs := make([]Transaction, 0, len(txIDs))
	for _, id := range txIDs {
		ops, err := wal.Replay(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("walrecover: replay %s: %w", id, err)
		}
		txs = append(txs, Transaction{ID: id, Ops: ops})
	}
	return txs, nil
}

func markApplied(ctx context.Context, wal types.WAL, batch []Transaction) error {
	if b, ok := wal.(BatchApplier); ok {
		ids := make([]types.TxID, len(batch))
		for i, t := range batch {
			ids[i] = t.ID
		}
		if err := b.MarkAppliedBatch(ctx, ids); err != nil {
			return fmt.Errorf("walrecover: mark applied: %w", err)
		}
		return nil
	}
	marker, ok := wal.(applier)
	if !ok {
		return nil
	}
	for _, t := range batch {
		if err := marker.MarkApplied(ctx, t.ID); err != nil {
			return fmt.Errorf("walrecover: mark applied %s: %w", t.ID, err)
		}
	}
	return nil
}

// recoveryTx treats a stale node write as already applied: replaying an old
// transaction must not fail because a later one already reached the store.
type recoveryTx struct{ types.StoreTx }

func (t recoveryTx) SaveNode(ctx context.Context, node *types.Node) error {
	err := t.StoreTx.SaveNode(ctx, node)
	if errors.Is(err, tree.ErrVersionConflict) {
		return nil
	}
	return err
}

// SaveActiveBranch forwards to the wrapped transaction when it can persist
// the active branch, and is a no-op otherwise, matching tree.ApplyOps.
func (t recoveryTx) SaveActiveBranch(ctx context.Context, branch types.BranchID) error {
	if w, ok := t.StoreTx.(tree.ActiveBranchWriter); ok {
		return w.SaveActiveBranch(ctx, branch)
	}
	return nil
}
