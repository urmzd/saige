package session

import "context"

// AccessStore holds explicit grants between sessions in the same tenant.
// Only a host's authenticated authorization path may grant access. Linking a
// session as evidence never creates a grant.
type AccessStore interface {
	GrantRead(context.Context, string, string, string) error
	RevokeRead(context.Context, string, string, string) error
	MayRead(context.Context, string, string, string) (bool, error)
}

func (m *Memory) GrantRead(ctx context.Context, tenant, target, reader string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.get(tenant, target); err != nil {
		return err
	}
	if _, err := m.get(tenant, reader); err != nil {
		return err
	}
	m.grants[scope(tenant, target)+"\x00"+reader] = true
	return nil
}
func (m *Memory) RevokeRead(ctx context.Context, tenant, target, reader string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.grants, scope(tenant, target)+"\x00"+reader)
	return nil
}
func (m *Memory) MayRead(ctx context.Context, tenant, target, reader string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, err := m.get(tenant, target); err != nil {
		return false, err
	}
	if _, err := m.get(tenant, reader); err != nil {
		return false, err
	}
	return m.grants[scope(tenant, target)+"\x00"+reader], nil
}
func (p *Postgres) GrantRead(ctx context.Context, tenant, target, reader string) error {
	if _, err := p.Get(ctx, tenant, target); err != nil {
		return err
	}
	if _, err := p.Get(ctx, tenant, reader); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO fiber_session_grants(tenant,target,reader) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, tenant, target, reader)
	return err
}
func (p *Postgres) RevokeRead(ctx context.Context, tenant, target, reader string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM fiber_session_grants WHERE tenant=$1 AND target=$2 AND reader=$3`, tenant, target, reader)
	return err
}
func (p *Postgres) MayRead(ctx context.Context, tenant, target, reader string) (bool, error) {
	if _, err := p.Get(ctx, tenant, target); err != nil {
		return false, err
	}
	if _, err := p.Get(ctx, tenant, reader); err != nil {
		return false, err
	}
	var granted bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fiber_session_grants WHERE tenant=$1 AND target=$2 AND reader=$3)`, tenant, target, reader).Scan(&granted)
	return granted, err
}
