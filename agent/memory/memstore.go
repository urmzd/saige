package memory

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemStore is an in-process Store, suitable as a test fixture. It is safe
// for concurrent use.
type MemStore struct {
	mu      sync.RWMutex
	records map[string]Record // scope key + "\x00" + id -> record
	now     func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{records: map[string]Record{}, now: time.Now}
}

// NewFixture returns a store preloaded with records, for evals and tests.
// Records without an ID get one derived from their content.
func NewFixture(records ...Record) *MemStore {
	s := NewMemStore()
	for _, r := range records {
		if r.ID == "" {
			r.ID = recordID(r)
		}
		if r.Kind == "" {
			r.Kind = KindSemantic
		}
		s.records[r.Scope.Key()+"\x00"+r.ID] = cloneRecord(r)
	}
	return s
}

func cloneRecord(r Record) Record {
	r.Tags = append([]string(nil), r.Tags...)
	return r
}

// Remember implements Store.
func (m *MemStore) Remember(ctx context.Context, r Record) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := r.Scope.Validate(); err != nil {
		return "", err
	}
	if r.Scope.ReadOnly {
		return "", ErrReadOnly
	}
	if r.ID == "" {
		r.ID = recordID(r)
	}
	if r.Kind == "" {
		r.Kind = KindSemantic
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = m.now().UTC()
	}
	key := r.Scope.Key() + "\x00" + r.ID
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[key]; ok && r.IdempotencyKey != "" {
		return r.ID, nil
	}
	r.Scope.ReadOnly = false
	m.records[key] = cloneRecord(r)
	return r.ID, nil
}

// Recall implements Store.
func (m *MemStore) Recall(ctx context.Context, s Scope, query string, budget int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	var in []Record
	for _, r := range m.records {
		if s.contains(r.Scope) {
			in = append(in, cloneRecord(r))
		}
	}
	m.mu.RUnlock()
	return rank(in, query, budget, m.now()), nil
}

// Forget implements Store.
func (m *MemStore) Forget(ctx context.Context, s Scope, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if s.ReadOnly {
		return ErrReadOnly
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, r := range m.records {
		if r.ID == id && s.contains(r.Scope) {
			delete(m.records, key)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}
