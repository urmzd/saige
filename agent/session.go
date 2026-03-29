package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Session represents a serializable agent conversation state.
type Session struct {
	ID        string            `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	TreeData  json.RawMessage   `json:"tree_data"`
	Metadata  map[string]any    `json:"metadata,omitempty"`
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
func (a *Agent) LoadSession(s *Session) error {
	if err := json.Unmarshal(s.TreeData, a.cfg.Tree); err != nil {
		return fmt.Errorf("unmarshal tree: %w", err)
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
