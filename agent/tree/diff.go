package tree

import (
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// DiffOp describes whether a node was added or removed between two tree states.
type DiffOp int

// Diff operations.
const (
	DiffAdded   DiffOp = iota // Node present in target but not in source
	DiffRemoved               // Node present in source but not in target
)

// NodeDiff describes a single node difference.
type NodeDiff struct {
	Op      DiffOp
	NodeID  types.NodeID
	Path    types.TreePath
	Message types.Message
	Depth   int
}

// TreeDiff describes the difference between two nodes in the tree.
type TreeDiff struct {
	CommonAncestor types.NodeID
	CommonPath     []types.NodeID
	Added          []NodeDiff
	Removed        []NodeDiff
}

// Diff computes the difference between two nodes by finding their common
// ancestor and reporting nodes unique to each path.
func (t *Tree) Diff(fromNodeID, toNodeID types.NodeID) (TreeDiff, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.diffUnlocked(fromNodeID, toNodeID)
}

func (t *Tree) diffUnlocked(fromNodeID, toNodeID types.NodeID) (TreeDiff, error) {
	fromPath, err := t.pathUnlocked(fromNodeID)
	if err != nil {
		return TreeDiff{}, err
	}
	toPath, err := t.pathUnlocked(toNodeID)
	if err != nil {
		return TreeDiff{}, err
	}

	// Find longest common prefix.
	commonLen := 0
	for i := 0; i < len(fromPath) && i < len(toPath); i++ {
		if fromPath[i] != toPath[i] {
			break
		}
		commonLen = i + 1
	}

	if commonLen == 0 {
		return TreeDiff{}, fmt.Errorf("nodes share no common ancestor")
	}

	commonPath := fromPath[:commonLen]
	commonAncestor := commonPath[commonLen-1]

	// Nodes in fromPath after the common prefix are "removed".
	var removed []NodeDiff
	for _, nid := range fromPath[commonLen:] {
		node := t.nodes[nid]
		tp, _ := t.nodePathUnlocked(nid)
		removed = append(removed, NodeDiff{
			Op:      DiffRemoved,
			NodeID:  nid,
			Path:    tp,
			Message: node.Message,
			Depth:   node.Depth,
		})
	}

	// Nodes in toPath after the common prefix are "added".
	var added []NodeDiff
	for _, nid := range toPath[commonLen:] {
		node := t.nodes[nid]
		tp, _ := t.nodePathUnlocked(nid)
		added = append(added, NodeDiff{
			Op:      DiffAdded,
			NodeID:  nid,
			Path:    tp,
			Message: node.Message,
			Depth:   node.Depth,
		})
	}

	return TreeDiff{
		CommonAncestor: commonAncestor,
		CommonPath:     commonPath,
		Added:          added,
		Removed:        removed,
	}, nil
}

// DiffBranches computes the difference between the tips of two branches.
func (t *Tree) DiffBranches(a, b types.BranchID) (TreeDiff, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tipA, okA := t.branches[a]
	if !okA {
		return TreeDiff{}, fmt.Errorf("%w: %s", ErrBranchNotFound, a)
	}
	tipB, okB := t.branches[b]
	if !okB {
		return TreeDiff{}, fmt.Errorf("%w: %s", ErrBranchNotFound, b)
	}
	return t.diffUnlocked(tipA, tipB)
}
