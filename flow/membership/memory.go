package membership

import (
	"context"
	"sync"
	"time"
)

// Memory is a Store in one process. The zero value is ready and has no
// members.
type Memory struct {
	mu   sync.RWMutex
	rows map[key]Member
	now  func() time.Time
}

type key struct{ tenant, channel, agent string }

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{} }

var _ Store = (*Memory)(nil)

// Join implements Store.
func (m *Memory) Join(ctx context.Context, tenant, channel, agent string, rule Rule) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validate(tenant, channel, agent); err != nil {
		return err
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = make(map[key]Member)
	}
	k := key{tenant, channel, agent}
	joined := now().UTC()
	if cur, ok := m.rows[k]; ok {
		joined = cur.Joined
	}
	m.rows[k] = Member{Tenant: tenant, Channel: channel, Agent: agent, Rule: cloneRule(rule), Joined: joined}
	return nil
}

// Leave implements Store.
func (m *Memory) Leave(ctx context.Context, tenant, channel, agent string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key{tenant, channel, agent}
	if _, ok := m.rows[k]; !ok {
		return ErrNotFound
	}
	delete(m.rows, k)
	return nil
}

// Members implements Store.
func (m *Memory) Members(ctx context.Context, tenant, channel string) ([]Member, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenant == "" || channel == "" {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []Member
	for k, row := range m.rows {
		if k.tenant == tenant {
			row.Rule = cloneRule(row.Rule)
			all = append(all, row)
		}
	}
	return pick(channel, all), nil
}
