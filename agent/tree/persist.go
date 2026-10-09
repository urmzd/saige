package tree

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// TxOpSetActive records the active-branch pointer. SetActive and Compact
// emit it, so a WAL replay or a WithStore write-through restores the active
// branch along with the branch tips.
const TxOpSetActive = types.TxOpSetActive

// ActiveBranchWriter is implemented by stores and store transactions that can
// persist the active-branch pointer. ApplyOps skips TxOpSetActive for a
// transaction that does not implement it.
type ActiveBranchWriter = types.ActiveBranchWriter

// ActiveBranchReader is implemented by stores that persist the active-branch
// pointer. LoadActiveBranch returns "" and no error when none was saved.
type ActiveBranchReader = types.ActiveBranchReader

// ApplyOps writes WAL ops to a store transaction by kind: node ops call
// SaveNode, branch ops call SaveBranch, checkpoint ops call SaveCheckpoint and
// TxOpSetActive calls SaveActiveBranch when tx supports it. Every write is an
// idempotent upsert, so applying the same ops twice is safe. It is the single
// mapping used by WithStore write-through and by WAL recovery.
func ApplyOps(ctx context.Context, tx types.StoreTx, ops []types.TxOp) error {
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
		case TxOpSetActive:
			if w, ok := tx.(ActiveBranchWriter); ok {
				if err := w.SaveActiveBranch(ctx, op.BranchID); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown op kind %q", op.Kind)
		}
	}
	return nil
}

// LoadFromStore rebuilds the tree rooted at rootID from store. Only
// checkpoints whose node belongs to the tree are kept. When active is empty,
// the store's saved active branch is used if the store implements
// ActiveBranchReader and that branch exists; otherwise "main". Options (for
// example WithWAL or WithStore) apply to the rebuilt tree.
func LoadFromStore(ctx context.Context, store types.Store, rootID types.NodeID, active types.BranchID, opts ...Option) (*Tree, error) {
	if store == nil {
		return nil, fmt.Errorf("tree: nil store")
	}
	nodes, branches, err := store.LoadTree(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("load tree: %w", err)
	}
	cps, err := store.ListCheckpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoints: %w", err)
	}
	inTree := make(map[types.NodeID]bool, len(nodes))
	for _, n := range nodes {
		inTree[n.ID] = true
	}
	checkpoints := make(map[types.CheckpointID]types.Checkpoint, len(cps))
	for _, cp := range cps {
		if inTree[cp.NodeID] {
			checkpoints[cp.ID] = cp
		}
	}
	if active == "" {
		if r, ok := store.(ActiveBranchReader); ok {
			saved, err := r.LoadActiveBranch(ctx)
			if err != nil {
				return nil, fmt.Errorf("load active branch: %w", err)
			}
			if tip, ok := branches[saved]; ok && inTree[tip] {
				active = saved
			}
		}
	}
	if active == "" {
		active = types.BranchID("main")
	}
	return FromStore(nodes, branches, checkpoints, rootID, active, opts...)
}
