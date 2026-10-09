package tree

import (
	"errors"

	"github.com/urmzd/saige/agent/types"
)

var (
	ErrNodeNotFound       = errors.New("node not found")
	ErrNodeArchived       = errors.New("node is archived")
	ErrVersionConflict    = types.ErrVersionConflict
	ErrInvalidBranchPoint = errors.New("invalid branch point")
	ErrCheckpointNotFound = errors.New("checkpoint not found")
	ErrBranchNotFound     = errors.New("branch not found")
	ErrRootImmutable      = errors.New("root node is immutable")
	ErrNodeIsLeaf         = errors.New("node is a permanent leaf and cannot have children")
	ErrInvalidRoot        = errors.New("root must be a SystemMessage")
	// ErrTreeFormatVersion is returned when a serialized tree names a format
	// version this package cannot read.
	ErrTreeFormatVersion = errors.New("unsupported tree format version")
	// ErrStoreWrite wraps a failed write-through to the store configured with
	// WithStore. The tree is unchanged when it is returned.
	ErrStoreWrite = errors.New("tree store write failed")
	// ErrWALCommit wraps a WAL commit that failed after the store configured
	// with WithStore accepted the same mutation. Unlike other errors, the
	// tree has applied the change, because the store already holds it, and
	// the method's other results are valid. Treat it as a warning that the
	// WAL no longer covers this mutation.
	ErrWALCommit = errors.New("tree WAL commit failed after store write")
)
