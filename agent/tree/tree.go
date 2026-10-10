package tree

import (
	"context"
	"encoding/json"
	"errors"
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

// DefaultPersistTimeout bounds each store write made through WithStore.
const DefaultPersistTimeout = 5 * time.Second

// WithStore makes every tree mutation write through to store. Each mutation
// applies its ops (the same ops a WAL records) in one store transaction
// before the in-memory tree changes, so a reloaded tree matches the live one:
// compaction branches, feedback leaves, edits, archive state, rewinds,
// checkpoints and the active branch all persist. A failed store write leaves
// the tree unchanged and returns an error wrapping ErrStoreWrite.
//
// Store writes run under context.WithoutCancel with DefaultPersistTimeout (see
// WithPersistTimeout): bookkeeping for work that already happened, such as a
// tool result, must still land when the caller's context is cancelled.
//
// When a WAL is also configured, the WAL transaction is opened first, the
// store transaction runs, and the WAL commit follows; a store failure aborts
// the WAL transaction so recovery never replays a mutation the caller saw fail.
// A WAL commit failure after the store write succeeded still applies the
// change to the tree, because the store already holds it, and returns an
// error wrapping ErrWALCommit alongside the normal result.
func WithStore(store types.Store) Option {
	return func(t *Tree) { t.store = store }
}

// WithPersistTimeout overrides DefaultPersistTimeout for WithStore writes. A
// non-positive value keeps the default.
func WithPersistTimeout(d time.Duration) Option {
	return func(t *Tree) {
		if d > 0 {
			t.persistTimeout = d
		}
	}
}

// Tree is a branching conversation graph rooted at a system message.
// Tree is safe for concurrent use; runs on different branches may overlap.
type Tree struct {
	mu          sync.RWMutex
	nodes       map[types.NodeID]*types.Node
	children    map[types.NodeID][]types.NodeID // parent -> ordered children
	rootID      types.NodeID
	branches    map[types.BranchID]types.NodeID // branch -> tip node
	active      types.BranchID                  // the branch Invoke reads from
	checkpoints map[types.CheckpointID]types.Checkpoint
	wal         types.WAL
	metadata    json.RawMessage

	store          types.Store
	persistTimeout time.Duration
}

// New creates a new conversation tree rooted at the given system message.
// It is NewContext with context.Background.
func New(systemMsg types.SystemMessage, opts ...Option) (*Tree, error) {
	return NewContext(context.Background(), systemMsg, opts...)
}

// NewContext creates a new conversation tree rooted at the given system
// message. ctx bounds the WAL write of the root node.
func NewContext(ctx context.Context, systemMsg types.SystemMessage, opts ...Option) (*Tree, error) {
	t := &Tree{
		nodes:          make(map[types.NodeID]*types.Node),
		children:       make(map[types.NodeID][]types.NodeID),
		branches:       make(map[types.BranchID]types.NodeID),
		checkpoints:    make(map[types.CheckpointID]types.Checkpoint),
		persistTimeout: DefaultPersistTimeout,
	}
	for _, opt := range opts {
		opt(t)
	}
	if err := validateMetadata(t.metadata); err != nil {
		return nil, err
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

	walErr := t.walAddNode(ctx, root)
	if !applied(walErr) {
		return nil, walErr
	}

	t.nodes[rootID] = root
	t.rootID = rootID
	t.branches[mainBranch] = rootID
	t.active = mainBranch

	return t, walErr
}

// Active returns the currently active branch ID.
func (t *Tree) Active() types.BranchID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active
}

// SetActive sets the active branch. Returns an error if the branch does not
// exist. It is SetActiveContext with context.Background.
func (t *Tree) SetActive(branch types.BranchID) error {
	return t.SetActiveContext(context.Background(), branch)
}

// SetActiveContext sets the active branch. Returns an error if the branch does
// not exist. ctx bounds the WAL write.
func (t *Tree) SetActiveContext(ctx context.Context, branch types.BranchID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tip, ok := t.branches[branch]
	if !ok {
		return fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	// Re-record the branch tip so WAL replay recreates the branch being
	// switched to even if its creating tx was already applied and pruned.
	walErr := t.walTx(ctx,
		types.TxOp{Kind: types.TxOpSetBranch, BranchID: branch, TipID: tip},
		types.TxOp{Kind: TxOpSetActive, BranchID: branch},
	)
	if !applied(walErr) {
		return walErr
	}
	t.active = branch
	return walErr
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
//
// The child joins the branch whose tip is parentID, so appending at the tip
// of a rewound branch extends that branch instead of the branch the parent
// was created on. When several branches end at parentID, the active branch
// wins, then the parent's own branch, then the lexically first. When no
// branch ends at parentID, the child joins the parent's branch.
//
// A node ID alone cannot say which of several branches sharing a tip the
// caller means. Right after a rewind to the active branch's current tip,
// both branches end at the same node and AddChild extends the active one.
// Use AddChildOnBranch whenever the caller knows the branch.
func (t *Tree) AddChild(ctx context.Context, parentID types.NodeID, msg types.Message) (*types.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, err := t.getNode(parentID)
	if err != nil {
		return nil, err
	}
	return t.addChildUnlocked(ctx, parent, t.branchEndingAt(parent), msg)
}

// AddChildOnBranch appends a message at the tip of branch and advances that
// branch. Prefer it over Tip plus AddChild when the caller knows the branch:
// it is atomic and never moves another branch that shares the tip node.
func (t *Tree) AddChildOnBranch(ctx context.Context, branch types.BranchID, msg types.Message) (*types.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	parent, err := t.getNode(tipID)
	if err != nil {
		return nil, err
	}
	return t.addChildUnlocked(ctx, parent, branch, msg)
}

// branchEndingAt picks the branch a new child of parent belongs to. Caller
// must hold the lock.
func (t *Tree) branchEndingAt(parent *types.Node) types.BranchID {
	if t.branches[t.active] == parent.ID {
		return t.active
	}
	if t.branches[parent.BranchID] == parent.ID {
		return parent.BranchID
	}
	var best types.BranchID
	for b, tip := range t.branches {
		if tip == parent.ID && (best == "" || b < best) {
			best = b
		}
	}
	if best != "" {
		return best
	}
	return parent.BranchID
}

// addChildUnlocked appends msg under parent on branch. Caller must hold the
// lock.
func (t *Tree) addChildUnlocked(ctx context.Context, parent *types.Node, branch types.BranchID, msg types.Message) (*types.Node, error) {
	parentID := parent.ID
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
		BranchID:  branch,
		CreatedAt: now,
		UpdatedAt: now,
	}

	walErr := t.walAddNode(ctx, child)
	if !applied(walErr) {
		return nil, walErr
	}

	t.nodes[child.ID] = child
	t.children[parentID] = append(t.children[parentID], child.ID)
	t.branches[child.BranchID] = child.ID

	return child, walErr
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

	walErr := t.walAddNode(ctx, child)
	if !applied(walErr) {
		return nil, walErr
	}

	t.nodes[child.ID] = child
	t.children[parentID] = append(t.children[parentID], child.ID)
	t.branches[branchID] = child.ID

	return child, walErr
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

	walErr := t.walAddNode(ctx, child)
	if !applied(walErr) {
		return "", nil, walErr
	}

	t.nodes[child.ID] = child
	t.children[fromNodeID] = append(t.children[fromNodeID], child.ID)
	t.branches[branchID] = child.ID

	return branchID, child, walErr
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

	walErr := t.walAddNode(ctx, child)
	if !applied(walErr) {
		return "", nil, walErr
	}

	t.nodes[child.ID] = child
	t.children[node.ParentID] = append(t.children[node.ParentID], child.ID)
	t.branches[branchID] = child.ID

	return branchID, child, walErr
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

// Node returns the node with the given ID.
func (t *Tree) Node(nodeID types.NodeID) (*types.Node, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.getNode(nodeID)
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

// Archive soft-deletes a node. If recursive is true, all descendants are also
// archived. It is ArchiveContext with context.Background.
func (t *Tree) Archive(nodeID types.NodeID, archivedBy string, recursive bool) error {
	return t.ArchiveContext(context.Background(), nodeID, archivedBy, recursive)
}

// ArchiveContext soft-deletes a node. If recursive is true, all descendants
// are also archived. ctx bounds the WAL write.
func (t *Tree) ArchiveContext(ctx context.Context, nodeID types.NodeID, archivedBy string, recursive bool) error {
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
	return t.mutateSubtree(ctx, node, recursive, func(mutated *types.Node) {
		mutated.State = types.NodeArchived
		mutated.ArchivedAt = &now
		mutated.ArchivedBy = archivedBy
		mutated.Version++
		mutated.UpdatedAt = now
	})
}

// Restore un-archives a node. If recursive is true, all descendants are also
// restored. It is RestoreContext with context.Background.
func (t *Tree) Restore(nodeID types.NodeID, recursive bool) error {
	return t.RestoreContext(context.Background(), nodeID, recursive)
}

// RestoreContext un-archives a node. If recursive is true, all descendants are
// also restored. ctx bounds the WAL write.
func (t *Tree) RestoreContext(ctx context.Context, nodeID types.NodeID, recursive bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return err
	}

	now := time.Now()
	return t.mutateSubtree(ctx, node, recursive, func(mutated *types.Node) {
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
func (t *Tree) mutateSubtree(ctx context.Context, node *types.Node, recursive bool, mutate func(*types.Node)) error {
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

	walErr := t.walTx(ctx, ops...)
	if !applied(walErr) {
		return walErr
	}

	// Copy the mutated state back through the original pointers so nodes held
	// by callers (and the tree's own maps) observe the update.
	for i, n := range targets {
		*n = *clones[i]
	}
	return walErr
}

// Checkpoint creates a named checkpoint at the current tip of a branch. It is
// CheckpointContext with context.Background.
func (t *Tree) Checkpoint(branch types.BranchID, name string) (types.CheckpointID, error) {
	return t.CheckpointContext(context.Background(), branch, name)
}

// CheckpointContext creates a named checkpoint at the current tip of a branch.
// It is covered by the WAL when one is configured and by the store when the
// tree was built WithStore; otherwise use Agent.Checkpoint (or call
// Store.SaveCheckpoint yourself) when the checkpoint must survive a
// store-only reload. ctx bounds the WAL write.
func (t *Tree) CheckpointContext(ctx context.Context, branch types.BranchID, name string) (types.CheckpointID, error) {
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

	walErr := t.walTx(ctx, types.TxOp{
		Kind: types.TxOpAddCheckpoint, Checkpoint: &cp,
	})
	if !applied(walErr) {
		return "", walErr
	}

	t.checkpoints[cpID] = cp

	return cpID, walErr
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

// Rewind creates a new branch starting from the checkpoint's node. It is
// RewindContext with context.Background.
func (t *Tree) Rewind(cp types.CheckpointID) (types.BranchID, error) {
	return t.RewindContext(context.Background(), cp)
}

// RewindContext creates a new branch whose tip is the checkpoint's node.
// AddChildOnBranch on the returned branch extends it and leaves the original
// branch untouched. AddChild at the checkpoint's node does the same unless
// the active branch also ends there (see AddChild); make the returned branch
// active first, or use AddChildOnBranch. ctx bounds the WAL write.
func (t *Tree) RewindContext(ctx context.Context, cp types.CheckpointID) (types.BranchID, error) {
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

	walErr := t.walTx(ctx, types.TxOp{
		Kind: types.TxOpSetBranch, BranchID: branchID, TipID: checkpoint.NodeID,
	})
	if !applied(walErr) {
		return "", walErr
	}

	t.branches[branchID] = checkpoint.NodeID

	return branchID, walErr
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

// walTx writes ops as a single WAL transaction if a WAL is configured, and as
// a single store transaction if a store is configured. Multi-node mutations
// (recursive archive, compaction) pass all their ops in one call so replay
// applies them atomically. Callers apply the in-memory change only when
// applied reports true for the result, and return the result as is.
//
// The WAL transaction is opened and filled under ctx, so a cancelled caller
// stops before anything is written. Once the store write has started, the
// rest runs under context.WithoutCancel: the store already holds the
// mutation, and the WAL must commit it so the two agree. If the WAL commit
// still fails after a successful store write, the error wraps ErrWALCommit:
// the store is the durable copy, so the tree applies the change too and stays
// in step with it.
func (t *Tree) walTx(ctx context.Context, ops ...types.TxOp) error {
	if len(ops) == 0 || (t.wal == nil && t.store == nil) {
		return nil
	}
	var txID types.TxID
	if t.wal != nil {
		var err error
		txID, err = t.wal.Begin(ctx)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if err := t.wal.Append(ctx, txID, op); err != nil {
				_ = t.wal.Abort(context.WithoutCancel(ctx), txID)
				return err
			}
		}
	}
	if t.store != nil {
		if err := t.persist(ctx, ops); err != nil {
			if t.wal != nil {
				_ = t.wal.Abort(context.WithoutCancel(ctx), txID)
			}
			return err
		}
		ctx = context.WithoutCancel(ctx)
	}
	if t.wal != nil {
		if err := t.wal.Commit(ctx, txID); err != nil {
			if t.store != nil {
				return fmt.Errorf("%w: %w", ErrWALCommit, err)
			}
			return err
		}
	}
	return nil
}

// applied reports whether a walTx result leaves the mutation durable, so the
// in-memory tree must apply it: either everything succeeded, or the store
// holds the change and only the WAL commit failed.
func applied(err error) bool {
	return err == nil || errors.Is(err, ErrWALCommit)
}

// persist applies ops to the configured store in one transaction, detached
// from ctx's cancellation and bounded by the persist timeout.
func (t *Tree) persist(ctx context.Context, ops []types.TxOp) error {
	timeout := t.persistTimeout
	if timeout <= 0 {
		timeout = DefaultPersistTimeout
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	err := t.store.Tx(pctx, func(tx types.StoreTx) error {
		return ApplyOps(pctx, tx, ops)
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStoreWrite, err)
	}
	return nil
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
		nodes:          make(map[types.NodeID]*types.Node, len(nodes)),
		children:       make(map[types.NodeID][]types.NodeID),
		branches:       make(map[types.BranchID]types.NodeID, len(branches)),
		checkpoints:    make(map[types.CheckpointID]types.Checkpoint, len(checkpoints)),
		rootID:         rootID,
		active:         active,
		persistTimeout: DefaultPersistTimeout,
	}
	for _, opt := range opts {
		opt(t)
	}
	if err := validateMetadata(t.metadata); err != nil {
		return nil, err
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
