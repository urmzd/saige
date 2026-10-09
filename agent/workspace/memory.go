package workspace

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Memory is an in-process Workspace. It is safe for concurrent use.
type Memory struct {
	mu      sync.RWMutex
	objects map[string][]byte // digest -> content
	names   map[string]Ref    // name -> latest ref
	now     func() time.Time
}

var _ Workspace = (*Memory)(nil)

// NewMemory returns an empty in-memory workspace.
func NewMemory() *Memory {
	return &Memory{objects: map[string][]byte{}, names: map[string]Ref{}, now: time.Now}
}

// Put implements Workspace.
func (m *Memory) Put(ctx context.Context, name string, data []byte, meta map[string]string) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if err := validName(name); err != nil {
		return Ref{}, err
	}
	id := Digest(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.names[name]; ok && prev.ID == id {
		return cloneRef(prev), nil
	}
	if _, ok := m.objects[id]; !ok {
		m.objects[id] = append([]byte(nil), data...)
	}
	ref := Ref{ID: id, Name: name, Size: int64(len(data)), Meta: copyMeta(meta), Created: m.now().UTC()}
	m.names[name] = ref
	return cloneRef(ref), nil
}

// Stat implements Workspace.
func (m *Memory) Stat(_ context.Context, ref Ref) (Ref, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, _, err := m.resolveLocked(ref)
	return r, err
}

func (m *Memory) resolveLocked(ref Ref) (Ref, []byte, error) {
	if ref.ID != "" {
		data, ok := m.objects[ref.ID]
		if !ok {
			return Ref{}, nil, fmt.Errorf("%w: %s", ErrNotFound, ref.URI())
		}
		// Prefer the named record when the ID is current under some name.
		// Several names can hold the same content; the first name in order
		// wins so the answer does not depend on map iteration.
		var best *Ref
		for _, r := range m.names {
			if r.ID == ref.ID && (ref.Name == "" || ref.Name == r.Name) && (best == nil || r.Name < best.Name) {
				r := r
				best = &r
			}
		}
		if best != nil {
			return cloneRef(*best), data, nil
		}
		return Ref{ID: ref.ID, Name: ref.Name, Size: int64(len(data))}, data, nil
	}
	if ref.Name == "" {
		return Ref{}, nil, ErrInvalidRef
	}
	r, ok := m.names[ref.Name]
	if !ok {
		return Ref{}, nil, fmt.Errorf("%w: %q", ErrNotFound, ref.Name)
	}
	return cloneRef(r), m.objects[r.ID], nil
}

// Read implements Workspace.
func (m *Memory) Read(ctx context.Context, ref Ref, offset, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, data, err := m.resolveLocked(ref)
	if err != nil {
		return nil, err
	}
	return window(data, offset, limit), nil
}

// List implements Workspace.
func (m *Memory) List(context.Context) ([]Ref, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Ref, 0, len(m.names))
	for _, r := range m.names {
		out = append(out, cloneRef(r))
	}
	sortRefs(out)
	return out, nil
}

// Search implements Workspace.
func (m *Memory) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	refs, _ := m.List(ctx)
	return searchRefs(ctx, refs, func(r Ref) ([]byte, error) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.objects[r.ID], nil
	}, query, k)
}

// View implements Workspace.
func (m *Memory) View(readOnly bool) Workspace { return view(m, readOnly) }
