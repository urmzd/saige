package tree

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("tree.branch_not_found", ErrBranchNotFound)
	agenttypes.RegisterWireSentinel("tree.checkpoint_not_found", ErrCheckpointNotFound)
	agenttypes.RegisterWireSentinel("tree.invalid_branch_point", ErrInvalidBranchPoint)
	agenttypes.RegisterWireSentinel("tree.invalid_root", ErrInvalidRoot)
	agenttypes.RegisterWireSentinel("tree.message_format_version", ErrMessageFormatVersion)
	agenttypes.RegisterWireSentinel("tree.node_archived", ErrNodeArchived)
	agenttypes.RegisterWireSentinel("tree.node_is_leaf", ErrNodeIsLeaf)
	agenttypes.RegisterWireSentinel("tree.node_not_found", ErrNodeNotFound)
	agenttypes.RegisterWireSentinel("tree.root_immutable", ErrRootImmutable)
	agenttypes.RegisterWireSentinel("tree.store_write", ErrStoreWrite)
	agenttypes.RegisterWireSentinel("tree.tree_format_version", ErrTreeFormatVersion)
	agenttypes.RegisterWireSentinel("tree.wal_commit", ErrWALCommit)
}
