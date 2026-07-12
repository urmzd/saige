package tree

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Option configures a Tree during construction.
type Option func(*Tree)

// WithWAL sets the write-ahead log for the tree.
func WithWAL(wal types.WAL) Option {
	return func(t *Tree) { t.wal = wal }
}

// Tree is a branching conversation graph rooted at a system message.
type Tree struct {
	mu          sync.RWMutex
	nodes       map[types.NodeID]*types.Node
	children    map[types.NodeID][]types.NodeID // parent -> ordered children
	rootID      types.NodeID
	branches    map[types.BranchID]types.NodeID // branch -> tip node
	active      types.BranchID                  // the branch Invoke reads from
	checkpoints map[types.CheckpointID]types.Checkpoint
	wal         types.WAL
}

// New creates a new conversation tree rooted at the given system message.
func New(systemMsg types.SystemMessage, opts ...Option) (*Tree, error) {
	t := &Tree{
		nodes:       make(map[types.NodeID]*types.Node),
		children:    make(map[types.NodeID][]types.NodeID),
		branches:    make(map[types.BranchID]types.NodeID),
		checkpoints: make(map[types.CheckpointID]types.Checkpoint),
	}
	for _, opt := range opts {
		opt(t)
	}

	rootID := types.NodeID(types.NewID())
	mainBranch := types.BranchID("main")
	now := time.Now()

	root := &types.Node{
		ID:        rootID,
		Message:   systemMsg,
		State:     types.NodeActive,
		Version:   1,
		Depth:     0,
		BranchID:  mainBranch,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(context.Background(), root); err != nil {
		return nil, err
	}

	t.nodes[rootID] = root
	t.rootID = rootID
	t.branches[mainBranch] = rootID
	t.active = mainBranch

	return t, nil
}

// Active returns the currently active branch ID.
func (t *Tree) Active() types.BranchID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active
}

// SetActive sets the active branch. Returns an error if the branch does not exist.
func (t *Tree) SetActive(branch types.BranchID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tip, ok := t.branches[branch]
	if !ok {
		return fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	// Re-record the branch tip so WAL replay recreates the branch being
	// switched to even if its creating tx was already applied and pruned.
	if err := t.walTx(context.Background(), types.TxOp{
		Kind: types.TxOpSetBranch, BranchID: branch, TipID: tip,
	}); err != nil {
		return err
	}
	t.active = branch
	return nil
}

// Root returns the root node.
func (t *Tree) Root() *types.Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodes[t.rootID]
}

// getNode returns a node by ID (caller must hold lock).
func (t *Tree) getNode(id types.NodeID) (*types.Node, error) {
	n, ok := t.nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, id)
	}
	return n, nil
}

// AddChild appends a message as a child of the given parent node.
func (t *Tree) AddChild(ctx context.Context, parentID types.NodeID, msg types.Message) (*types.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, err := t.getNode(parentID)
	if err != nil {
		return nil, err
	}
	if parent.State == types.NodeArchived {
		return nil, fmt.Errorf("%w: %s", ErrNodeArchived, parentID)
	}
	if parent.State == types.NodeFeedback {
		return nil, fmt.Errorf("%w: %s", ErrNodeIsLeaf, parentID)
	}

	now := time.Now()
	child := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  parentID,
		Message:   msg,
		State:     types.NodeActive,
		Version:   1,
		Depth:     parent.Depth + 1,
		BranchID:  parent.BranchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(ctx, child); err != nil {
		return nil, err
	}

	t.nodes[child.ID] = child
	t.children[parentID] = append(t.children[parentID], child.ID)
	t.branches[child.BranchID] = child.ID

	return child, nil
}

// AddFeedback appends a feedback message as a permanent leaf child of the
// given node. The child is on its own dead-end branch and cannot have
// further children added to it.
func (t *Tree) AddFeedback(ctx context.Context, parentID types.NodeID, msg types.Message) (*types.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, err := t.getNode(parentID)
	if err != nil {
		return nil, err
	}
	if parent.State == types.NodeArchived {
		return nil, fmt.Errorf("%w: %s", ErrNodeArchived, parentID)
	}
	if parent.State == types.NodeFeedback {
		return nil, fmt.Errorf("%w: %s", ErrNodeIsLeaf, parentID)
	}

	branchID := types.BranchID(fmt.Sprintf("feedback-%s", types.NewID()[:8]))
	now := time.Now()
	child := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  parentID,
		Message:   msg,
		State:     types.NodeFeedback,
		Version:   1,
		Depth:     parent.Depth + 1,
		BranchID:  branchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(ctx, child); err != nil {
		return nil, err
	}

	t.nodes[child.ID] = child
	t.children[parentID] = append(t.children[parentID], child.ID)
	t.branches[branchID] = child.ID

	return child, nil
}

// Feedback returns all feedback nodes in the tree.
func (t *Tree) Feedback() []*types.Node {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var nodes []*types.Node
	for _, n := range t.nodes {
		if n.State == types.NodeFeedback {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Branch creates a new branch diverging from the given node.
func (t *Tree) Branch(ctx context.Context, fromNodeID types.NodeID, name string, msg types.Message) (types.BranchID, *types.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	from, err := t.getNode(fromNodeID)
	if err != nil {
		return "", nil, err
	}
	if from.State == types.NodeArchived {
		return "", nil, fmt.Errorf("%w: %s", ErrNodeArchived, fromNodeID)
	}
	if from.State == types.NodeFeedback {
		return "", nil, fmt.Errorf("%w: %s", ErrNodeIsLeaf, fromNodeID)
	}

	branchID := types.BranchID(name)
	if _, exists := t.branches[branchID]; exists {
		branchID = types.BranchID(fmt.Sprintf("%s-%s", name, types.NewID()[:8]))
	}

	now := time.Now()
	child := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  fromNodeID,
		Message:   msg,
		State:     types.NodeActive,
		Version:   1,
		Depth:     from.Depth + 1,
		BranchID:  branchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(ctx, child); err != nil {
		return "", nil, err
	}

	t.nodes[child.ID] = child
	t.children[fromNodeID] = append(t.children[fromNodeID], child.ID)
	t.branches[branchID] = child.ID

	return branchID, child, nil
}

// UpdateUserMessage edits a user message by creating a new branch from the parent.
func (t *Tree) UpdateUserMessage(ctx context.Context, nodeID types.NodeID, newMsg types.UserMessage) (types.BranchID, *types.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return "", nil, err
	}
	if node.ParentID == "" {
		return "", nil, fmt.Errorf("%w: cannot update root", ErrRootImmutable)
	}
	if _, ok := node.Message.(types.UserMessage); !ok {
		return "", nil, fmt.Errorf("%w: node is not a user message", ErrInvalidBranchPoint)
	}

	parent, err := t.getNode(node.ParentID)
	if err != nil {
		return "", nil, err
	}

	branchID := types.BranchID(fmt.Sprintf("edit-%s", types.NewID()[:8]))
	now := time.Now()
	child := &types.Node{
		ID:        types.NodeID(types.NewID()),
		ParentID:  node.ParentID,
		Message:   newMsg,
		State:     types.NodeActive,
		Version:   1,
		Depth:     parent.Depth + 1,
		BranchID:  branchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(ctx, child); err != nil {
		return "", nil, err
	}

	t.nodes[child.ID] = child
	t.children[node.ParentID] = append(t.children[node.ParentID], child.ID)
	t.branches[branchID] = child.ID

	return branchID, child, nil
}

// Tip returns the tip node of the given branch.
func (t *Tree) Tip(branch types.BranchID) (*types.Node, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	return t.getNode(tipID)
}

// Path returns the node IDs from root to the given node.
func (t *Tree) Path(nodeID types.NodeID) ([]types.NodeID, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pathUnlocked(nodeID)
}

func (t *Tree) pathUnlocked(nodeID types.NodeID) ([]types.NodeID, error) {
	var path []types.NodeID
	current := nodeID
	for current != "" {
		node, err := t.getNode(current)
		if err != nil {
			return nil, err
		}
		path = append(path, current)
		current = node.ParentID
	}
	// Reverse to get root-first order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

// Children returns the child nodes of the given node.
func (t *Tree) Children(nodeID types.NodeID) ([]*types.Node, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if _, err := t.getNode(nodeID); err != nil {
		return nil, err
	}

	childIDs := t.children[nodeID]
	result := make([]*types.Node, 0, len(childIDs))
	for _, cid := range childIDs {
		if n, ok := t.nodes[cid]; ok {
			result = append(result, n)
		}
	}
	return result, nil
}

// Branches returns a copy of the branch-to-tip mapping.
func (t *Tree) Branches() map[types.BranchID]types.NodeID {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[types.BranchID]types.NodeID, len(t.branches))
	for k, v := range t.branches {
		result[k] = v
	}
	return result
}

// Archive soft-deletes a node. If recursive is true, all descendants are also archived.
func (t *Tree) Archive(nodeID types.NodeID, archivedBy string, recursive bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return err
	}
	if node.ParentID == "" {
		return fmt.Errorf("%w: cannot archive root", ErrRootImmutable)
	}

	now := time.Now()
	return t.mutateSubtree(node, recursive, func(mutated *types.Node) {
		mutated.State = types.NodeArchived
		mutated.ArchivedAt = &now
		mutated.ArchivedBy = archivedBy
		mutated.Version++
		mutated.UpdatedAt = now
	})
}

// Restore un-archives a node. If recursive is true, all descendants are also restored.
func (t *Tree) Restore(nodeID types.NodeID, recursive bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return err
	}

	now := time.Now()
	return t.mutateSubtree(node, recursive, func(mutated *types.Node) {
		mutated.State = types.NodeActive
		mutated.ArchivedAt = nil
		mutated.ArchivedBy = ""
		mutated.Version++
		mutated.UpdatedAt = now
	})
}

// collectSubtree returns node and (when recursive) all its descendants,
// depth-first. Caller must hold the lock.
func (t *Tree) collectSubtree(node *types.Node, recursive bool) []*types.Node {
	nodes := []*types.Node{node}
	if recursive {
		for _, childID := range t.children[node.ID] {
			if child, ok := t.nodes[childID]; ok {
				nodes = append(nodes, t.collectSubtree(child, true)...)
			}
		}
	}
	return nodes
}

// mutateSubtree applies mutate to node (and its descendants when recursive),
// writing all resulting states as TxOpUpdateNode ops in a single WAL
// transaction BEFORE the in-memory tree is touched, so the log never lags a
// partially applied multi-node mutation. Caller must hold the lock.
func (t *Tree) mutateSubtree(node *types.Node, recursive bool, mutate func(*types.Node)) error {
	targets := t.collectSubtree(node, recursive)

	ops := make([]types.TxOp, 0, len(targets))
	clones := make([]*types.Node, 0, len(targets))
	for _, n := range targets {
		clone := *n
		mutate(&clone)
		c := clone
		clones = append(clones, &c)
		ops = append(ops, types.TxOp{Kind: types.TxOpUpdateNode, NodeID: n.ID, Node: &c})
	}

	if err := t.walTx(context.Background(), ops...); err != nil {
		return err
	}

	// Copy the mutated state back through the original pointers so nodes held
	// by callers (and the tree's own maps) observe the update.
	for i, n := range targets {
		*n = *clones[i]
	}
	return nil
}

// Checkpoint creates a named checkpoint at the current tip of a branch. It is
// covered by the WAL when one is configured, but is NOT written to a Store:
// use Agent.Checkpoint (or call Store.SaveCheckpoint yourself) when the
// checkpoint must survive a store-only reload.
func (t *Tree) Checkpoint(branch types.BranchID, name string) (types.CheckpointID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}

	cpID := types.CheckpointID(types.NewID())
	cp := types.Checkpoint{
		ID:        cpID,
		Branch:    branch,
		NodeID:    tipID,
		Name:      name,
		CreatedAt: time.Now(),
	}

	if err := t.walTx(context.Background(), types.TxOp{
		Kind: types.TxOpAddCheckpoint, Checkpoint: &cp,
	}); err != nil {
		return "", err
	}

	t.checkpoints[cpID] = cp

	return cpID, nil
}

// Checkpoints returns a copy of all checkpoints in the tree.
func (t *Tree) Checkpoints() map[types.CheckpointID]types.Checkpoint {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[types.CheckpointID]types.Checkpoint, len(t.checkpoints))
	for k, v := range t.checkpoints {
		result[k] = v
	}
	return result
}

// Rewind creates a new branch starting from the checkpoint's node.
func (t *Tree) Rewind(cp types.CheckpointID) (types.BranchID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	checkpoint, ok := t.checkpoints[cp]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrCheckpointNotFound, cp)
	}

	if _, err := t.getNode(checkpoint.NodeID); err != nil {
		return "", err
	}

	branchID := types.BranchID(fmt.Sprintf("rewind-%s-%s", checkpoint.Name, types.NewID()[:8]))

	if err := t.walTx(context.Background(), types.TxOp{
		Kind: types.TxOpSetBranch, BranchID: branchID, TipID: checkpoint.NodeID,
	}); err != nil {
		return "", err
	}

	t.branches[branchID] = checkpoint.NodeID

	return branchID, nil
}

// NodePath returns the TreePath (child indices from root) for the given node.
func (t *Tree) NodePath(nodeID types.NodeID) (types.TreePath, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodePathUnlocked(nodeID)
}

func (t *Tree) nodePathUnlocked(nodeID types.NodeID) (types.TreePath, error) {
	nodePath, err := t.pathUnlocked(nodeID)
	if err != nil {
		return nil, err
	}
	if len(nodePath) <= 1 {
		return types.TreePath{}, nil // root has empty path
	}

	treePath := make(types.TreePath, 0, len(nodePath)-1)
	for i := 1; i < len(nodePath); i++ {
		parentID := nodePath[i-1]
		childID := nodePath[i]
		siblings := t.children[parentID]
		found := false
		for idx, sid := range siblings {
			if sid == childID {
				treePath = append(treePath, idx)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("child %s not found in parent %s children", childID, parentID)
		}
	}
	return treePath, nil
}

// walTx writes ops as a single WAL transaction if a WAL is configured.
// Multi-node mutations (recursive archive, compaction) pass all their ops in
// one call so replay applies them atomically.
func (t *Tree) walTx(ctx context.Context, ops ...types.TxOp) error {
	if t.wal == nil || len(ops) == 0 {
		return nil
	}
	txID, err := t.wal.Begin(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := t.wal.Append(ctx, txID, op); err != nil {
			_ = t.wal.Abort(ctx, txID)
			return err
		}
	}
	return t.wal.Commit(ctx, txID)
}

// walAddNode writes a node addition and its branch-tip move as one WAL
// transaction. Every node insertion also moves (or creates) the tip of the
// node's branch, so the two ops are inseparable for replay.
func (t *Tree) walAddNode(ctx context.Context, node *types.Node) error {
	return t.walTx(ctx,
		types.TxOp{Kind: types.TxOpAddNode, NodeID: node.ID, ParentID: node.ParentID, Node: node},
		types.TxOp{Kind: types.TxOpSetBranch, BranchID: node.BranchID, TipID: node.ID},
	)
}

// FromStore reconstructs a Tree from persisted data (e.g. from pgstore.LoadTree).
// The nodes slice must contain at least the root. The children map and active
// branch are inferred from the node data and branches map. Options (e.g.
// WithWAL) apply to the reconstructed tree so recovered sessions keep
// write-ahead protection for subsequent mutations.
func FromStore(
	nodes []*types.Node,
	branches map[types.BranchID]types.NodeID,
	checkpoints map[types.CheckpointID]types.Checkpoint,
	rootID types.NodeID,
	active types.BranchID,
	opts ...Option,
) (*Tree, error) {
	if len(nodes) == 0 {
		return nil, ErrNodeNotFound
	}

	t := &Tree{
		nodes:       make(map[types.NodeID]*types.Node, len(nodes)),
		children:    make(map[types.NodeID][]types.NodeID),
		branches:    make(map[types.BranchID]types.NodeID, len(branches)),
		checkpoints: make(map[types.CheckpointID]types.Checkpoint, len(checkpoints)),
		rootID:      rootID,
		active:      active,
	}
	for _, opt := range opts {
		opt(t)
	}

	for _, n := range nodes {
		t.nodes[n.ID] = n
	}

	// Rebuild children map from parent pointers.
	for _, n := range nodes {
		if n.ParentID != "" {
			t.children[n.ParentID] = append(t.children[n.ParentID], n.ID)
		}
	}

	for bid, nid := range branches {
		t.branches[bid] = nid
	}

	for cpID, cp := range checkpoints {
		t.checkpoints[cpID] = cp
	}

	return t, nil
}
