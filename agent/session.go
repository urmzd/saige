package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// Session represents a serializable agent conversation state.
type Session struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	TreeData  json.RawMessage `json:"tree_data"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// SaveSession serializes the current agent's conversation tree to a Session.
func (a *Agent) SaveSession() (*Session, error) {
	treeData, err := json.Marshal(a.cfg.Tree)
	if err != nil {
		return nil, fmt.Errorf("marshal tree: %w", err)
	}

	now := time.Now()
	return &Session{
		ID:        types.NewID(),
		CreatedAt: now,
		UpdatedAt: now,
		TreeData:  treeData,
		Metadata: map[string]any{
			"agent_name": a.cfg.Name,
		},
	}, nil
}

// LoadSession restores an agent's conversation from a Session.
//
// The session is decoded and checked in full before the live tree changes:
// every branch tip and checkpoint must resolve to a path from the root. A
// session that fails either step returns an error and leaves the current
// conversation untouched. Loading while a run is active on the tree returns
// ErrRunActive.
func (a *Agent) LoadSession(s *Session) error {
	if s == nil {
		return fmt.Errorf("load session: nil session")
	}
	// The tree claim is held through the final unmarshal so no run can start
	// and write to the tree while its contents are being replaced.
	release, err := claimTree(a.cfg.Tree)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	defer release()
	var scratch tree.Tree
	if err := json.Unmarshal(s.TreeData, &scratch); err != nil {
		return fmt.Errorf("unmarshal tree: %w", err)
	}
	if err := validateLoadedTree(&scratch); err != nil {
		return fmt.Errorf("invalid session tree: %w", err)
	}
	if err := json.Unmarshal(s.TreeData, a.cfg.Tree); err != nil {
		return fmt.Errorf("unmarshal tree: %w", err)
	}
	return nil
}

// validateLoadedTree checks the structure a decoded tree must have before it
// can replace a live one. It walks down from the root first, so a parent
// cycle or a node that names the wrong parent is caught before any walk up a
// parent chain could loop.
func validateLoadedTree(t *tree.Tree) error {
	root := t.Root()
	if root == nil {
		return fmt.Errorf("root node is missing")
	}
	if root.ParentID != "" {
		return fmt.Errorf("root node %s has a parent", root.ID)
	}
	reachable := map[types.NodeID]bool{root.ID: true}
	queue := []types.NodeID{root.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		children, err := t.Children(id)
		if err != nil {
			return fmt.Errorf("node %s: %w", id, err)
		}
		for _, child := range children {
			if child.ParentID != id {
				return fmt.Errorf("node %s is listed under %s but names parent %s", child.ID, id, child.ParentID)
			}
			if reachable[child.ID] {
				return fmt.Errorf("node %s is reachable twice", child.ID)
			}
			reachable[child.ID] = true
			queue = append(queue, child.ID)
		}
	}
	branches := t.Branches()
	if _, ok := branches[t.Active()]; !ok {
		return fmt.Errorf("active branch %q does not exist", t.Active())
	}
	for branch, tip := range branches {
		if !reachable[tip] {
			return fmt.Errorf("branch %q tip %s is not in the tree", branch, tip)
		}
		if _, err := t.FlattenBranch(branch); err != nil {
			return fmt.Errorf("branch %q: %w", branch, err)
		}
	}
	for id, cp := range t.Checkpoints() {
		if !reachable[cp.NodeID] {
			return fmt.Errorf("checkpoint %q node %s is not in the tree", id, cp.NodeID)
		}
	}
	return nil
}

// SaveSessionToFile saves a session to a JSON file.
func SaveSessionToFile(s *Session, path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadSessionFromFile loads a session from a JSON file.
func LoadSessionFromFile(path string) (*Session, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed internally, not from user input
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}
	return &s, nil
}
