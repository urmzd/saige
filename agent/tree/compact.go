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
		shared = t.sharedNodesUnlocked(branch)
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

	summary, err := summarizeMessages(ctx, provider, msgs)
	if err != nil {
		return "", err
	}

	// Create a new branch forking from the parent of the first compacted node.
	first := toCompact[0]
	last := toCompact[len(toCompact)-1]

	newBranchID := types.BranchID(fmt.Sprintf("compact-%s-%s", branch, types.NewID()[:8]))

	// The summary is stored as a user+assistant pair: the summary text is
	// model-generated, so it belongs on an assistant turn, but some providers
	// reject histories whose first non-system message is from the assistant,
	// so a synthetic user request precedes it.
	now := time.Now()
	requestNode := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  first.node.ParentID,
		Message:   types.NewUserMessage(types.SummaryRequestText),
		State:     types.NodeCompacted,
		Version:   1,
		Depth:     first.node.Depth,
		BranchID:  newBranchID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	summaryNode := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  requestNode.ID,
		Message:   types.NewAssistantMessage(summary),
		State:     types.NodeCompacted,
		Version:   1,
		Depth:     first.node.Depth + 1,
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

	// Build the new branch's nodes up front (summary pair + clones of the
	// remaining suffix) so the whole compaction lands in ONE WAL transaction
	// before the in-memory tree changes.
	newNodes := []*types.Node{requestNode, summaryNode}
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

// sharedNodesUnlocked returns the set of node IDs reachable from any branch
// other than the given one. Caller must hold t.mu.
func (t *Tree) sharedNodesUnlocked(branch types.BranchID) map[types.NodeID]bool {
	shared := make(map[types.NodeID]bool)
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
	return shared
}

// summarizeMessages asks the provider to summarize msgs and returns the
// summary text, failing on stream errors or an empty result.
func summarizeMessages(ctx context.Context, provider types.Provider, msgs []types.Message) (string, error) {
	summaryReq := []types.Message{
		types.NewSystemMessage("Summarize the following conversation concisely, preserving key facts and decisions."),
		types.NewUserMessage(types.MessagesToText(msgs)),
	}

	rx, err := provider.ChatStream(ctx, summaryReq, nil)
	if err != nil {
		return "", fmt.Errorf("summarization: %w", err)
	}

	var summaryBuf strings.Builder
	var streamErr error
	for delta := range rx {
		switch d := delta.(type) {
		case types.TextContentDelta:
			summaryBuf.WriteString(d.Content)
		case types.ErrorDelta:
			// Mid-stream failures (e.g. rate limits after the stream opened)
			// arrive as ErrorDelta, not as the ChatStream return error.
			streamErr = d.Error
		}
	}
	if streamErr != nil {
		return "", fmt.Errorf("summarization: %w", streamErr)
	}
	summary := summaryBuf.String()
	if strings.TrimSpace(summary) == "" {
		// An empty summary would silently drop the compacted messages.
		return "", fmt.Errorf("summarization produced an empty summary")
	}
	return summary, nil
}
