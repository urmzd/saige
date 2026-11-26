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

// MemoryStore is a Store in process memory. Once it holds MaxScopes
// conversations it forgets the one used longest ago.
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
	list, ok := m.scopes[scope]
	if ok {
		m.touchLocked(scope)
	}
	return slices.Clone(list), nil
}

// touchLocked marks scope as used last.
func (m *MemoryStore) touchLocked(scope string) {
	if i := slices.Index(m.order, scope); i >= 0 {
		m.order = append(slices.Delete(m.order, i, i+1), scope)
	}
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
	if ok {
		m.touchLocked(scope)
	}
	if !slices.Contains(list, tool) {
		m.scopes[scope] = append(list, tool)
	}
	return nil
}
