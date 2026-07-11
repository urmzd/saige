// Package memstore implements agent/types.Store using in-memory maps.
//
// It is a drop-in, dependency-free Store suitable for tests and single-process
// use. Unlike pgstore it offers no crash durability — data lives only for the
// lifetime of the process — but it mirrors the full types.Store contract so
// persistence and tree-reconstruction paths can be exercised without Postgres.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

var (
	_ types.Store   = (*Store)(nil)
	_ types.StoreTx = (*storeTx)(nil)
)

// Store is an in-memory implementation of types.Store.
//
// All operations are safe for concurrent use. Nodes are copied on the way in
// and out so callers cannot mutate stored state by holding a returned pointer.
type Store struct {
	mu          sync.RWMutex
	nodes       map[types.NodeID]*types.Node
	childOrder  map[types.NodeID][]types.NodeID // parent -> children in insertion order
	branches    map[types.BranchID]types.NodeID
	checkpoints map[types.CheckpointID]types.Checkpoint
}

// New creates an empty in-memory Store.
func New() *Store {
	return &Store{
		nodes:       make(map[types.NodeID]*types.Node),
		childOrder:  make(map[types.NodeID][]types.NodeID),
		branches:    make(map[types.BranchID]types.NodeID),
		checkpoints: make(map[types.CheckpointID]types.Checkpoint),
	}
}

// cloneNode returns a defensive copy of a node so stored state is immutable
// from the caller's perspective. The Message itself is treated as immutable
// (the tree builds a fresh message per node), so it is shared by reference.
func cloneNode(n *types.Node) *types.Node {
	cp := *n
	if n.SummaryOf != nil {
		cp.SummaryOf = append([]types.NodeID(nil), n.SummaryOf...)
	}
	if n.ArchivedAt != nil {
		at := *n.ArchivedAt
		cp.ArchivedAt = &at
	}
	return &cp
}

// saveNode is the shared write path used by both Store and storeTx.
func (s *Store) saveNode(node *types.Node) error {
	if node.ID == "" {
		return fmt.Errorf("memstore: node ID is empty")
	}
	_, existed := s.nodes[node.ID]
	s.nodes[node.ID] = cloneNode(node)
	if !existed && node.ParentID != "" {
		s.childOrder[node.ParentID] = append(s.childOrder[node.ParentID], node.ID)
	}
	return nil
}

// SaveNode persists a node, preserving child insertion order on first write.
func (s *Store) SaveNode(_ context.Context, node *types.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveNode(node)
}

// LoadNode retrieves a single node by ID.
func (s *Store) LoadNode(_ context.Context, id types.NodeID) (*types.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil, fmt.Errorf("node not found: %s", id)
	}
	return cloneNode(n), nil
}

// LoadChildren returns direct children of a node in insertion order.
func (s *Store) LoadChildren(_ context.Context, parentID types.NodeID) ([]*types.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.childOrder[parentID]
	out := make([]*types.Node, 0, len(ids))
	for _, id := range ids {
		if n, ok := s.nodes[id]; ok {
			out = append(out, cloneNode(n))
		}
	}
	return out, nil
}

// LoadPath returns all nodes from root to the given node, root-first.
func (s *Store) LoadPath(_ context.Context, toNodeID types.NodeID) ([]*types.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var path []*types.Node
	current := toNodeID
	for current != "" {
		n, ok := s.nodes[current]
		if !ok {
			return nil, fmt.Errorf("node not found: %s", current)
		}
		path = append(path, cloneNode(n))
		current = n.ParentID
	}
	// Reverse to root-first order.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

// SaveBranch persists a branch-to-tip mapping.
func (s *Store) SaveBranch(_ context.Context, branch types.BranchID, tipID types.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branches[branch] = tipID
	return nil
}

// LoadBranch retrieves the tip node ID for a branch.
func (s *Store) LoadBranch(_ context.Context, branch types.BranchID) (types.NodeID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tip, ok := s.branches[branch]
	if !ok {
		return "", fmt.Errorf("branch not found: %s", branch)
	}
	return tip, nil
}

// ListBranches returns a copy of all branch-to-tip mappings.
func (s *Store) ListBranches(_ context.Context) (map[types.BranchID]types.NodeID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[types.BranchID]types.NodeID, len(s.branches))
	for k, v := range s.branches {
		out[k] = v
	}
	return out, nil
}

// SaveCheckpoint persists a checkpoint.
func (s *Store) SaveCheckpoint(_ context.Context, cp types.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoints[cp.ID] = cp
	return nil
}

// LoadCheckpoint retrieves a checkpoint by ID.
func (s *Store) LoadCheckpoint(_ context.Context, id types.CheckpointID) (types.Checkpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp, ok := s.checkpoints[id]
	if !ok {
		return types.Checkpoint{}, fmt.Errorf("checkpoint not found: %s", id)
	}
	return cp, nil
}

// ListCheckpoints returns all checkpoints ordered by creation time, then ID.
func (s *Store) ListCheckpoints(_ context.Context) ([]types.Checkpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]types.Checkpoint, 0, len(s.checkpoints))
	for _, cp := range s.checkpoints {
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// LoadTree returns every node reachable from rootID plus all branch tips.
// Nodes are ordered root-first (ascending depth), matching pgstore semantics
// closely enough for tree.FromStore, which rebuilds the child map from parent
// pointers regardless of slice order.
func (s *Store) LoadTree(_ context.Context, rootID types.NodeID) ([]*types.Node, map[types.BranchID]types.NodeID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.nodes[rootID]; !ok {
		return nil, nil, fmt.Errorf("root not found: %s", rootID)
	}

	// BFS from the root over the child-order map so only the reachable subtree
	// is returned (mirrors pgstore's recursive descendant query).
	var nodes []*types.Node
	queue := []types.NodeID{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n, ok := s.nodes[id]
		if !ok {
			continue
		}
		nodes = append(nodes, cloneNode(n))
		queue = append(queue, s.childOrder[id]...)
	}

	// Stable order: ascending depth, then created time, for deterministic output.
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].CreatedAt.Before(nodes[j].CreatedAt)
	})

	branches := make(map[types.BranchID]types.NodeID, len(s.branches))
	for k, v := range s.branches {
		branches[k] = v
	}
	return nodes, branches, nil
}

// storeTx implements types.StoreTx by buffering writes and applying them
// atomically on commit. If fn returns an error, no buffered writes are applied.
type storeTx struct {
	store       *Store
	nodes       []*types.Node
	branches    map[types.BranchID]types.NodeID
	checkpoints []types.Checkpoint
}

func (t *storeTx) SaveNode(_ context.Context, node *types.Node) error {
	if node.ID == "" {
		return fmt.Errorf("memstore: node ID is empty")
	}
	t.nodes = append(t.nodes, cloneNode(node))
	return nil
}

func (t *storeTx) SaveBranch(_ context.Context, branch types.BranchID, tipID types.NodeID) error {
	t.branches[branch] = tipID
	return nil
}

func (t *storeTx) SaveCheckpoint(_ context.Context, cp types.Checkpoint) error {
	t.checkpoints = append(t.checkpoints, cp)
	return nil
}

// Tx runs fn against a buffered transaction, applying its writes atomically on
// success. On error nothing is persisted, giving all-or-nothing semantics that
// match pgstore's database transaction.
func (s *Store) Tx(ctx context.Context, fn func(types.StoreTx) error) error {
	tx := &storeTx{
		store:    s,
		branches: make(map[types.BranchID]types.NodeID),
	}
	if err := fn(tx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range tx.nodes {
		if err := s.saveNode(n); err != nil {
			return err
		}
	}
	for b, tip := range tx.branches {
		s.branches[b] = tip
	}
	for _, cp := range tx.checkpoints {
		s.checkpoints[cp.ID] = cp
	}
	return nil
}
