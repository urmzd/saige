package types

import "context"

// TxID uniquely identifies a WAL transaction.
type TxID string

// TxOpKind describes the type of operation in a transaction.
type TxOpKind string

const (
	TxOpAddNode       TxOpKind = "add_node"
	TxOpUpdateNode    TxOpKind = "update_node"
	TxOpSetBranch     TxOpKind = "set_branch"
	TxOpAddChild      TxOpKind = "add_child"
	TxOpAddCheckpoint TxOpKind = "add_checkpoint"
	// TxOpSetActive records the active-branch pointer. Its BranchID names
	// the branch that Invoke reads from.
	TxOpSetActive TxOpKind = "set_active"
)

// TxOp is a single operation within a WAL transaction.
type TxOp struct {
	Kind       TxOpKind
	NodeID     NodeID
	ParentID   NodeID
	Node       *Node
	BranchID   BranchID
	TipID      NodeID
	Checkpoint *Checkpoint
}

// WAL provides write-ahead logging for atomic tree mutations.
type WAL interface {
	Begin(ctx context.Context) (TxID, error)
	Append(ctx context.Context, txID TxID, op TxOp) error
	Commit(ctx context.Context, txID TxID) error
	Abort(ctx context.Context, txID TxID) error
	Recover(ctx context.Context) ([]TxID, error)
	Replay(ctx context.Context, txID TxID) ([]TxOp, error)
}
