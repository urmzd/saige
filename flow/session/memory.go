package session

import (
	"context"
	"github.com/urmzd/saige/flow/event"
	"slices"
	"strings"
	"sync"
	"time"
)

type Memory struct {
	mu           sync.RWMutex
	rows         map[string]Record
	links        map[string][]Link
	runs         map[string][]string
	reservations map[string][]Reservation
	grants       map[string]bool
	accounts     map[string]Account
	usage        map[string]event.RunUsage
	usageSeen    map[string]bool
}

func NewMemory() *Memory {
	return &Memory{rows: map[string]Record{}, links: map[string][]Link{}, runs: map[string][]string{}, reservations: map[string][]Reservation{}, grants: map[string]bool{}, accounts: map[string]Account{}, usage: map[string]event.RunUsage{}, usageSeen: map[string]bool{}}
}
func scope(tenant, id string) string { return tenant + "\x00" + id }
func (m *Memory) Open(ctx context.Context, tenant string, o Open) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	k, err := prepare(tenant, o)
	if err != nil {
		return Record{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := k.ID()
	if r, ok := m.rows[scope(tenant, id)]; ok {
		return r, nil
	}
	depth := 0
	if strings.HasPrefix(k.Parent, "s_") {
		p, ok := m.rows[scope(tenant, k.Parent)]
		if !ok {
			return Record{}, ErrNotFound
		}
		depth = p.Depth + 1
	}
	r := Record{ID: id, Key: k, Channel: channel(k, o.Channel), Depth: depth, CreatedAt: time.Now().UTC()}
	m.rows[scope(tenant, id)] = r
	return r, nil
}
func (m *Memory) get(tenant, id string) (Record, error) {
	if tenant == "" {
		return Record{}, ErrNotFound
	}
	r, ok := m.rows[scope(tenant, id)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}
func (m *Memory) Get(ctx context.Context, tenant, id string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.get(tenant, id)
}
func (m *Memory) List(ctx context.Context, tenant string, f Filter) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenant == "" {
		return nil, ErrInvalid
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Record{}
	for _, r := range m.rows {
		if r.Key.Tenant == tenant && (f.Parent == "" || r.Key.Parent == f.Parent) && (f.Agent == "" || r.Key.Agent == f.Agent) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Record) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out[:min(len(out), limit(f.Limit))], nil
}
func (m *Memory) Link(ctx context.Context, tenant, id string, l Link) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validLink(l) || l.Target == id {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.get(tenant, id); err != nil {
		return err
	}
	if _, err := m.get(tenant, l.Target); err != nil {
		return err
	}
	key := scope(tenant, id)
	for _, x := range m.links[key] {
		if x.Target == l.Target && x.Kind == l.Kind {
			return nil
		}
	}
	l.CreatedAt = time.Now().UTC()
	m.links[key] = append(m.links[key], l)
	return nil
}
func (m *Memory) Links(ctx context.Context, tenant, id string, n int) ([]Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, err := m.get(tenant, id); err != nil {
		return nil, err
	}
	xs := m.links[scope(tenant, id)]
	return append([]Link{}, xs[:min(len(xs), limit(n))]...), nil
}
func (m *Memory) BindRun(ctx context.Context, tenant, id, run string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == "" {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.get(tenant, id); err != nil {
		return err
	}
	key := scope(tenant, id)
	if !slices.Contains(m.runs[key], run) {
		m.runs[key] = append(m.runs[key], run)
	}
	return nil
}
func (m *Memory) Runs(ctx context.Context, tenant, id string, n int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, err := m.get(tenant, id); err != nil {
		return nil, err
	}
	xs := m.runs[scope(tenant, id)]
	start := max(0, len(xs)-limit(n))
	return append([]string{}, xs[start:]...), nil
}
