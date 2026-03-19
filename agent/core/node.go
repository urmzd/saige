package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TreePath is a hierarchical child-index path from root to a node.
// Each element is the child index at that level. For example, [0, 1, 2]
// means "root's 0th child → that node's 1st child → that node's 2nd child".
type TreePath []int

// String returns the path as "0/1/2". Returns "" for an empty (root) path.
func (p TreePath) String() string {
	if len(p) == 0 {
		return ""
	}
	parts := make([]string, len(p))
	for i, idx := range p {
		parts[i] = strconv.Itoa(idx)
	}
	return strings.Join(parts, "/")
}

// ParseTreePath parses a path string like "0/1/2" back into a TreePath.
// An empty string returns an empty (root) TreePath.
func ParseTreePath(s string) (TreePath, error) {
	if s == "" {
		return TreePath{}, nil
	}
	parts := strings.Split(s, "/")
	path := make(TreePath, len(parts))
	for i, p := range parts {
		idx, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid tree path segment %q: %w", p, err)
		}
		path[i] = idx
	}
	return path, nil
}

// Parent returns the parent path. For [0,1,2] returns [0,1].
// Returns nil for an empty or single-element path.
func (p TreePath) Parent() TreePath {
	if len(p) <= 1 {
		return nil
	}
	cp := make(TreePath, len(p)-1)
	copy(cp, p[:len(p)-1])
	return cp
}

// IsAncestorOf returns true if p is a strict prefix of other.
func (p TreePath) IsAncestorOf(other TreePath) bool {
	if len(p) >= len(other) {
		return false
	}
	for i, idx := range p {
		if other[i] != idx {
			return false
		}
	}
	return true
}

// Tokenizer counts tokens for overflow detection.
// Implementations wrap provider-specific tokenizer endpoints.
type Tokenizer interface {
	CountTokens(ctx context.Context, messages []Message) (int, error)
}

// NodeID uniquely identifies a node in the conversation tree.
type NodeID string

// BranchID uniquely identifies a branch in the conversation tree.
type BranchID string

// CheckpointID uniquely identifies a checkpoint.
type CheckpointID string

// NodeState represents the lifecycle state of a node.
type NodeState int

const (
	NodeActive    NodeState = iota // Normal, visible node
	NodeArchived                   // Soft-deleted
	NodeCompacted                  // Replaced by a summary
	NodeFeedback                   // Permanent leaf — cannot have children
)

// Node is a single message in the conversation tree.
type Node struct {
	ID         NodeID
	ParentID   NodeID // empty for root
	Message    Message
	State      NodeState
	Version    uint64 // optimistic concurrency control
	Depth      int    // distance from root
	BranchID   BranchID
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ArchivedAt *time.Time
	ArchivedBy string
	SummaryOf  []NodeID // populated when State == NodeCompacted
}

// Checkpoint records a named snapshot of a branch tip.
type Checkpoint struct {
	ID        CheckpointID
	Branch    BranchID
	NodeID    NodeID
	Name      string
	CreatedAt time.Time
}
