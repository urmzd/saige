package tree

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// CompactOpts configures tree-aware compaction.
type CompactOpts struct {
	MaxTokens      int  // context window budget
	PreserveShared bool // don't compact shared ancestors (default true)
}

// Compact compresses a branch by summarizing an eligible prefix of messages
// when the total token count exceeds MaxTokens. Instead of mutating the branch
// in-place, it creates a new compacted branch and sets it as active.
// Returns the new branch ID, or the original branch if no compaction was needed.
func (t *Tree) Compact(ctx context.Context, branch types.BranchID, provider types.Provider, tokenizer types.Tokenizer, opts CompactOpts) (types.BranchID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}

	path, err := t.pathUnlocked(tipID)
	if err != nil {
		return "", err
	}

	// Collect messages for token counting.
	messages := make([]types.Message, 0, len(path))
	for _, nid := range path {
		node := t.nodes[nid]
		if node.State == types.NodeArchived {
			continue
		}
		messages = append(messages, node.Message)
	}

	// Check if we're over budget.
	tokenCount, err := tokenizer.CountTokens(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("counting tokens: %w", err)
	}
	if tokenCount <= opts.MaxTokens {
		return branch, nil // under budget, no compaction needed
	}

	// Identify nodes shared across other branches if PreserveShared.
	shared := make(map[types.NodeID]bool)
	if opts.PreserveShared {
		for brID, brTip := range t.branches {
			if brID == branch {
				continue
			}
			brPath, err := t.pathUnlocked(brTip)
			if err != nil {
				continue
			}
			for _, nid := range brPath {
				shared[nid] = true
			}
		}
	}

	// Build list of active, non-root, non-shared node IDs on the path.
	type candidate struct {
		id   types.NodeID
		node *types.Node
	}
	var candidates []candidate
	for _, nid := range path {
		node := t.nodes[nid]
		if node.ParentID == "" {
			continue // never compact root
		}
		if node.State != types.NodeActive {
			continue
		}
		if opts.PreserveShared && shared[nid] {
			continue
		}
		candidates = append(candidates, candidate{id: nid, node: node})
	}

	if len(candidates) == 0 {
		return branch, nil
	}

	// Compact the first half of candidates (or at least 1).
	compactCount := len(candidates) / 2
	if compactCount < 1 {
		compactCount = 1
	}
	toCompact := candidates[:compactCount]

	// Summarize the run via provider.
	msgs := make([]types.Message, 0, len(toCompact))
	nodeIDs := make([]types.NodeID, 0, len(toCompact))
	for _, c := range toCompact {
		msgs = append(msgs, c.node.Message)
		nodeIDs = append(nodeIDs, c.id)
	}

	summaryText := types.MessagesToText(msgs)
	summaryReq := []types.Message{
		types.NewSystemMessage("Summarize the following conversation concisely, preserving key facts and decisions."),
		types.NewUserMessage(summaryText),
	}

	rx, err := provider.ChatStream(ctx, summaryReq, nil)
	if err != nil {
		return "", fmt.Errorf("summarization: %w", err)
	}

	var summaryBuf strings.Builder
	for delta := range rx {
		if tc, ok := delta.(types.TextContentDelta); ok {
			summaryBuf.WriteString(tc.Content)
		}
	}
	summary := summaryBuf.String()

	// Create a new branch forking from the parent of the first compacted node.
	first := toCompact[0]
	last := toCompact[len(toCompact)-1]

	newBranchID := types.BranchID(fmt.Sprintf("compact-%s-%s", branch, types.NewID()[:8]))

	now := time.Now()
	summaryNode := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  first.node.ParentID,
		Message:   types.NewUserMessage("Summary of previous conversation: " + summary),
		State:     types.NodeCompacted,
		Version:   1,
		Depth:     first.node.Depth,
		BranchID:  newBranchID,
		CreatedAt: now,
		UpdatedAt: now,
		SummaryOf: nodeIDs,
	}

	// Re-link remaining (non-compacted) nodes after the compacted prefix onto the new branch.
	var remaining []types.NodeID
	pastCompacted := false
	for _, nid := range path {
		if nid == last.id {
			pastCompacted = true
			continue
		}
		if pastCompacted {
			remaining = append(remaining, nid)
		}
	}

	// Build the new branch's nodes up front (summary + clones of the remaining
	// suffix) so the whole compaction lands in ONE WAL transaction before the
	// in-memory tree changes.
	newNodes := []*types.Node{summaryNode}
	prevID := summaryNode.ID
	newTipID := summaryNode.ID
	for _, nid := range remaining {
		orig := t.nodes[nid]
		cloned := &types.Node{
			ID:        types.NodeID(types.NewID()),
			ParentID:  prevID,
			Message:   orig.Message,
			State:     types.NodeActive,
			Version:   1,
			Depth:     orig.Depth,
			BranchID:  newBranchID,
			CreatedAt: orig.CreatedAt,
			UpdatedAt: now,
		}
		newNodes = append(newNodes, cloned)
		prevID = cloned.ID
		newTipID = cloned.ID
	}

	ops := make([]types.TxOp, 0, len(newNodes)+1)
	for _, n := range newNodes {
		ops = append(ops, types.TxOp{Kind: types.TxOpAddNode, NodeID: n.ID, ParentID: n.ParentID, Node: n})
	}
	ops = append(ops, types.TxOp{Kind: types.TxOpSetBranch, BranchID: newBranchID, TipID: newTipID})
	if err := t.walTx(ctx, ops...); err != nil {
		return "", err
	}

	for _, n := range newNodes {
		t.nodes[n.ID] = n
		t.children[n.ParentID] = append(t.children[n.ParentID], n.ID)
	}

	t.branches[newBranchID] = newTipID
	t.active = newBranchID

	return newBranchID, nil
}
