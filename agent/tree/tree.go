package tree

import (
	"fmt"
	"sync"
	"time"

	"github.com/urmzd/graph-agent-dev-kit/agent/core"
)

// Option configures a Tree during construction.
type Option func(*Tree)

// WithWAL sets the write-ahead log for the tree.
func WithWAL(wal core.WAL) Option {
	return func(t *Tree) { t.wal = wal }
}

// WithStore sets the persistence store for the tree.
func WithStore(store core.Store) Option {
	return func(t *Tree) { t.store = store }
}

// Tree is a branching conversation graph rooted at a system message.
type Tree struct {
	mu          sync.RWMutex
	nodes       map[core.NodeID]*core.Node
	children    map[core.NodeID][]core.NodeID // parent -> ordered children
	rootID      core.NodeID
	branches    map[core.BranchID]core.NodeID // branch -> tip node
	active      core.BranchID                 // the branch Invoke reads from
	checkpoints map[core.CheckpointID]core.Checkpoint
	wal         core.WAL
	store       core.Store
}

// New creates a new conversation tree rooted at the given system message.
func New(systemMsg core.SystemMessage, opts ...Option) (*Tree, error) {
	t := &Tree{
		nodes:       make(map[core.NodeID]*core.Node),
		children:    make(map[core.NodeID][]core.NodeID),
		branches:    make(map[core.BranchID]core.NodeID),
		checkpoints: make(map[core.CheckpointID]core.Checkpoint),
	}
	for _, opt := range opts {
		opt(t)
	}

	rootID := core.NodeID(core.NewID())
	mainBranch := core.BranchID("main")
	now := time.Now()

	root := &core.Node{
		ID:        rootID,
		Message:   systemMsg,
		State:     core.NodeActive,
		Version:   1,
		Depth:     0,
		BranchID:  mainBranch,
		CreatedAt: now,
		UpdatedAt: now,
	}

	t.nodes[rootID] = root
	t.rootID = rootID
	t.branches[mainBranch] = rootID
	t.active = mainBranch

	return t, nil
}

// Active returns the currently active branch ID.
func (t *Tree) Active() core.BranchID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active
}

// SetActive sets the active branch. Returns an error if the branch does not exist.
func (t *Tree) SetActive(branch core.BranchID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.branches[branch]; !ok {
		return fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	t.active = branch
	return nil
}

// Root returns the root node.
func (t *Tree) Root() *core.Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodes[t.rootID]
}

// getNode returns a node by ID (caller must hold lock).
func (t *Tree) getNode(id core.NodeID) (*core.Node, error) {
	n, ok := t.nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, id)
	}
	return n, nil
}

// AddChild appends a message as a child of the given parent node.
func (t *Tree) AddChild(parentID core.NodeID, msg core.Message) (*core.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, err := t.getNode(parentID)
	if err != nil {
		return nil, err
	}
	if parent.State == core.NodeArchived {
		return nil, fmt.Errorf("%w: %s", ErrNodeArchived, parentID)
	}
	if parent.State == core.NodeFeedback {
		return nil, fmt.Errorf("%w: %s", ErrNodeIsLeaf, parentID)
	}

	now := time.Now()
	child := &core.Node{
		ID:        core.NodeID(core.NewID()),
		ParentID:  parentID,
		Message:   msg,
		State:     core.NodeActive,
		Version:   1,
		Depth:     parent.Depth + 1,
		BranchID:  parent.BranchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(child); err != nil {
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
func (t *Tree) AddFeedback(parentID core.NodeID, msg core.Message) (*core.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, err := t.getNode(parentID)
	if err != nil {
		return nil, err
	}
	if parent.State == core.NodeArchived {
		return nil, fmt.Errorf("%w: %s", ErrNodeArchived, parentID)
	}
	if parent.State == core.NodeFeedback {
		return nil, fmt.Errorf("%w: %s", ErrNodeIsLeaf, parentID)
	}

	branchID := core.BranchID(fmt.Sprintf("feedback-%s", core.NewID()[:8]))
	now := time.Now()
	child := &core.Node{
		ID:        core.NodeID(core.NewID()),
		ParentID:  parentID,
		Message:   msg,
		State:     core.NodeFeedback,
		Version:   1,
		Depth:     parent.Depth + 1,
		BranchID:  branchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(child); err != nil {
		return nil, err
	}

	t.nodes[child.ID] = child
	t.children[parentID] = append(t.children[parentID], child.ID)
	t.branches[branchID] = child.ID

	return child, nil
}

// Feedback returns all feedback nodes in the tree.
func (t *Tree) Feedback() []*core.Node {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var nodes []*core.Node
	for _, n := range t.nodes {
		if n.State == core.NodeFeedback {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Branch creates a new branch diverging from the given node.
func (t *Tree) Branch(fromNodeID core.NodeID, name string, msg core.Message) (core.BranchID, *core.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	from, err := t.getNode(fromNodeID)
	if err != nil {
		return "", nil, err
	}
	if from.State == core.NodeArchived {
		return "", nil, fmt.Errorf("%w: %s", ErrNodeArchived, fromNodeID)
	}
	if from.State == core.NodeFeedback {
		return "", nil, fmt.Errorf("%w: %s", ErrNodeIsLeaf, fromNodeID)
	}

	branchID := core.BranchID(name)
	if _, exists := t.branches[branchID]; exists {
		branchID = core.BranchID(fmt.Sprintf("%s-%s", name, core.NewID()[:8]))
	}

	now := time.Now()
	child := &core.Node{
		ID:        core.NodeID(core.NewID()),
		ParentID:  fromNodeID,
		Message:   msg,
		State:     core.NodeActive,
		Version:   1,
		Depth:     from.Depth + 1,
		BranchID:  branchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(child); err != nil {
		return "", nil, err
	}

	t.nodes[child.ID] = child
	t.children[fromNodeID] = append(t.children[fromNodeID], child.ID)
	t.branches[branchID] = child.ID

	return branchID, child, nil
}

// UpdateUserMessage edits a user message by creating a new branch from the parent.
func (t *Tree) UpdateUserMessage(nodeID core.NodeID, newMsg core.UserMessage) (core.BranchID, *core.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return "", nil, err
	}
	if node.ParentID == "" {
		return "", nil, fmt.Errorf("%w: cannot update root", ErrRootImmutable)
	}
	if _, ok := node.Message.(core.UserMessage); !ok {
		return "", nil, fmt.Errorf("%w: node is not a user message", ErrInvalidBranchPoint)
	}

	parent, err := t.getNode(node.ParentID)
	if err != nil {
		return "", nil, err
	}

	branchID := core.BranchID(fmt.Sprintf("edit-%s", core.NewID()[:8]))
	now := time.Now()
	child := &core.Node{
		ID:        core.NodeID(core.NewID()),
		ParentID:  node.ParentID,
		Message:   newMsg,
		State:     core.NodeActive,
		Version:   1,
		Depth:     parent.Depth + 1,
		BranchID:  branchID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := t.walAddNode(child); err != nil {
		return "", nil, err
	}

	t.nodes[child.ID] = child
	t.children[node.ParentID] = append(t.children[node.ParentID], child.ID)
	t.branches[branchID] = child.ID

	return branchID, child, nil
}

// Tip returns the tip node of the given branch.
func (t *Tree) Tip(branch core.BranchID) (*core.Node, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	return t.getNode(tipID)
}

// Path returns the node IDs from root to the given node.
func (t *Tree) Path(nodeID core.NodeID) ([]core.NodeID, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pathUnlocked(nodeID)
}

func (t *Tree) pathUnlocked(nodeID core.NodeID) ([]core.NodeID, error) {
	var path []core.NodeID
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
func (t *Tree) Children(nodeID core.NodeID) ([]*core.Node, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if _, err := t.getNode(nodeID); err != nil {
		return nil, err
	}

	childIDs := t.children[nodeID]
	result := make([]*core.Node, 0, len(childIDs))
	for _, cid := range childIDs {
		if n, ok := t.nodes[cid]; ok {
			result = append(result, n)
		}
	}
	return result, nil
}

// Branches returns a copy of the branch-to-tip mapping.
func (t *Tree) Branches() map[core.BranchID]core.NodeID {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[core.BranchID]core.NodeID, len(t.branches))
	for k, v := range t.branches {
		result[k] = v
	}
	return result
}

// Archive soft-deletes a node. If recursive is true, all descendants are also archived.
func (t *Tree) Archive(nodeID core.NodeID, archivedBy string, recursive bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return err
	}
	if node.ParentID == "" {
		return fmt.Errorf("%w: cannot archive root", ErrRootImmutable)
	}

	return t.archiveNode(node, archivedBy, recursive)
}

func (t *Tree) archiveNode(node *core.Node, archivedBy string, recursive bool) error {
	now := time.Now()
	node.State = core.NodeArchived
	node.ArchivedAt = &now
	node.ArchivedBy = archivedBy
	node.Version++
	node.UpdatedAt = now

	if recursive {
		for _, childID := range t.children[node.ID] {
			child, err := t.getNode(childID)
			if err != nil {
				return err
			}
			if err := t.archiveNode(child, archivedBy, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restore un-archives a node. If recursive is true, all descendants are also restored.
func (t *Tree) Restore(nodeID core.NodeID, recursive bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := t.getNode(nodeID)
	if err != nil {
		return err
	}

	return t.restoreNode(node, recursive)
}

func (t *Tree) restoreNode(node *core.Node, recursive bool) error {
	now := time.Now()
	node.State = core.NodeActive
	node.ArchivedAt = nil
	node.ArchivedBy = ""
	node.Version++
	node.UpdatedAt = now

	if recursive {
		for _, childID := range t.children[node.ID] {
			child, err := t.getNode(childID)
			if err != nil {
				return err
			}
			if err := t.restoreNode(child, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// Checkpoint creates a named checkpoint at the current tip of a branch.
func (t *Tree) Checkpoint(branch core.BranchID, name string) (core.CheckpointID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tipID, ok := t.branches[branch]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}

	cpID := core.CheckpointID(core.NewID())
	cp := core.Checkpoint{
		ID:        cpID,
		Branch:    branch,
		NodeID:    tipID,
		Name:      name,
		CreatedAt: time.Now(),
	}
	t.checkpoints[cpID] = cp

	return cpID, nil
}

// Rewind creates a new branch starting from the checkpoint's node.
func (t *Tree) Rewind(cp core.CheckpointID) (core.BranchID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	checkpoint, ok := t.checkpoints[cp]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrCheckpointNotFound, cp)
	}

	if _, err := t.getNode(checkpoint.NodeID); err != nil {
		return "", err
	}

	branchID := core.BranchID(fmt.Sprintf("rewind-%s-%s", checkpoint.Name, core.NewID()[:8]))
	t.branches[branchID] = checkpoint.NodeID

	return branchID, nil
}

// NodePath returns the TreePath (child indices from root) for the given node.
func (t *Tree) NodePath(nodeID core.NodeID) (core.TreePath, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodePathUnlocked(nodeID)
}

func (t *Tree) nodePathUnlocked(nodeID core.NodeID) (core.TreePath, error) {
	nodePath, err := t.pathUnlocked(nodeID)
	if err != nil {
		return nil, err
	}
	if len(nodePath) <= 1 {
		return core.TreePath{}, nil // root has empty path
	}

	treePath := make(core.TreePath, 0, len(nodePath)-1)
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

// walAddNode writes a node addition to the WAL if configured.
func (t *Tree) walAddNode(node *core.Node) error {
	if t.wal == nil {
		return nil
	}
	txID, err := t.wal.Begin()
	if err != nil {
		return err
	}
	if err := t.wal.Append(txID, core.TxOp{Kind: core.TxOpAddNode, Node: node}); err != nil {
		_ = t.wal.Abort(txID)
		return err
	}
	return t.wal.Commit(txID)
}
