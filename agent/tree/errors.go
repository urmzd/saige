package tree

import "errors"

var (
	ErrNodeNotFound       = errors.New("node not found")
	ErrNodeArchived       = errors.New("node is archived")
	ErrVersionConflict    = errors.New("version conflict")
	ErrInvalidBranchPoint = errors.New("invalid branch point")
	ErrCheckpointNotFound = errors.New("checkpoint not found")
	ErrBranchNotFound     = errors.New("branch not found")
	ErrRootImmutable      = errors.New("root node is immutable")
	ErrNodeIsLeaf         = errors.New("node is a permanent leaf and cannot have children")
	ErrInvalidRoot        = errors.New("root must be a SystemMessage")
)
