package tree

import (
	"fmt"
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// AnnotatedMessage pairs a message with its tree metadata.
type AnnotatedMessage struct {
	Message  types.Message
	NodeID   types.NodeID
	Path     types.TreePath
	Depth    int
	BranchID types.BranchID
	State    types.NodeState
}

// Flatten walks the path from root to the given node and collects messages.
// Archived nodes are skipped. Compacted nodes contribute their summary message.
func (t *Tree) Flatten(toNodeID types.NodeID) ([]types.Message, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.flattenUnlocked(toNodeID)
}

func (t *Tree) flattenUnlocked(toNodeID types.NodeID) ([]types.Message, error) {
	// Depth is a capacity hint; parent links determine the actual history.
	capacity := 0
	if tip := t.nodes[toNodeID]; tip != nil {
		capacity = min(max(tip.Depth, 0), len(t.nodes)-1) + 1
	}
	messages := make([]types.Message, 0, capacity)
	for id := toNodeID; id != ""; {
		node, err := t.getNode(id)
		if err != nil {
			return nil, err
		}
		if node.State != types.NodeArchived {
			messages = append(messages, node.Message)
		}
		id = node.ParentID
	}
	slices.Reverse(messages)
	return messages, nil
}

// FlattenBranch flattens the path from root to the tip of the given branch.
func (t *Tree) FlattenBranch(branch types.BranchID) ([]types.Message, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	return t.flattenUnlocked(tipID)
}

// FlattenAnnotated walks the path from root to the given node and returns
// annotated messages with full metadata. Archived nodes are skipped.
// Compacted nodes are included with State: NodeCompacted.
func (t *Tree) FlattenAnnotated(toNodeID types.NodeID) ([]AnnotatedMessage, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.flattenAnnotatedUnlocked(toNodeID)
}

func (t *Tree) flattenAnnotatedUnlocked(toNodeID types.NodeID) ([]AnnotatedMessage, error) {
	path, err := t.pathUnlocked(toNodeID)
	if err != nil {
		return nil, err
	}

	result := make([]AnnotatedMessage, 0, len(path))
	for _, nid := range path {
		node := t.nodes[nid]
		if node.State == types.NodeArchived {
			continue
		}
		tp, err := t.nodePathUnlocked(nid)
		if err != nil {
			return nil, err
		}
		result = append(result, AnnotatedMessage{
			Message:  node.Message,
			NodeID:   node.ID,
			Path:     tp,
			Depth:    node.Depth,
			BranchID: node.BranchID,
			State:    node.State,
		})
	}
	return result, nil
}

// FlattenBranchAnnotated flattens the path from root to the tip of the given
// branch, returning annotated messages.
func (t *Tree) FlattenBranchAnnotated(branch types.BranchID) ([]AnnotatedMessage, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	return t.flattenAnnotatedUnlocked(tipID)
}
