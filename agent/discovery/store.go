package discovery

import (
	"context"
	"slices"
	"sync"
)

// Store keeps the held tools each conversation loaded, by scope key
// (agent.RunScope.Key). A host whose runs resume in another process passes
// a durable Store, so a resumed run is offered what it loaded before.
type Store interface {
	// Loaded returns the tools scope loaded.
	Loaded(ctx context.Context, scope string) ([]string, error)
	// Add records that scope loaded tool. Adding a tool twice is not an
	// error.
	Add(ctx context.Context, scope, tool string) error
}

// MemoryStore is a Store in process memory. It forgets the oldest
// conversation first once it holds MaxScopes of them.
type MemoryStore struct {
	// MaxScopes bounds how many conversations it keeps. Zero uses
	// DefaultMaxScopes.
	MaxScopes int

	mu     sync.Mutex
	scopes map[string][]string
	order  []string
}

// Loaded implements Store.
func (m *MemoryStore) Loaded(_ context.Context, scope string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.scopes[scope]), nil
}

// Add implements Store.
func (m *MemoryStore) Add(_ context.Context, scope, tool string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scopes == nil {
		m.scopes = map[string][]string{}
	}
	list, ok := m.scopes[scope]
	if !ok {
		limit := m.MaxScopes
		if limit <= 0 {
			limit = DefaultMaxScopes
		}
		for len(m.order) >= limit {
			delete(m.scopes, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, scope)
	}
	if !slices.Contains(list, tool) {
		m.scopes[scope] = append(list, tool)
	}
	return nil
}
