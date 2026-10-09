package types

import (
	"context"
	"errors"
)

// Store provides persistence for conversation tree data.
type Store interface {
	SaveNode(ctx context.Context, node *Node) error
	LoadNode(ctx context.Context, id NodeID) (*Node, error)
	LoadChildren(ctx context.Context, parentID NodeID) ([]*Node, error)
	LoadPath(ctx context.Context, toNodeID NodeID) ([]*Node, error)
	SaveBranch(ctx context.Context, branch BranchID, tipID NodeID) error
	LoadBranch(ctx context.Context, branch BranchID) (NodeID, error)
	ListBranches(ctx context.Context) (map[BranchID]NodeID, error)
	SaveCheckpoint(ctx context.Context, cp Checkpoint) error
	LoadCheckpoint(ctx context.Context, id CheckpointID) (Checkpoint, error)
	// ListCheckpoints returns every persisted checkpoint so a reloaded tree can
	// round-trip Checkpoint/Rewind (tree.FromStore takes a checkpoints map).
	ListCheckpoints(ctx context.Context) ([]Checkpoint, error)
	LoadTree(ctx context.Context, rootID NodeID) ([]*Node, map[BranchID]NodeID, error)
	Tx(ctx context.Context, fn func(StoreTx) error) error
}

// StoreTx is a transactional subset of Store operations.
type StoreTx interface {
	SaveNode(ctx context.Context, node *Node) error
	SaveBranch(ctx context.Context, branch BranchID, tipID NodeID) error
	SaveCheckpoint(ctx context.Context, cp Checkpoint) error
}

// ErrVersionConflict reports a node write whose version is older than the
// stored one.
var ErrVersionConflict = errors.New("version conflict")

// ActiveBranchWriter is implemented by stores and store transactions that can
// persist the active-branch pointer.
type ActiveBranchWriter interface {
	SaveActiveBranch(ctx context.Context, branch BranchID) error
}

// ActiveBranchReader is implemented by stores that persist the active-branch
// pointer. LoadActiveBranch returns "" and no error when none was saved.
type ActiveBranchReader interface {
	LoadActiveBranch(ctx context.Context) (BranchID, error)
}

// ConversationDeleter is implemented by stores that can delete everything
// they hold for their conversation.
type ConversationDeleter interface {
	DeleteConversation(ctx context.Context) error
}
