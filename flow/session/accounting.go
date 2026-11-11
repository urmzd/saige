package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/flow/event"
)

// UsageStore counts each actual model call once in its session and ancestors.
// It never adds run totals, because those may already contain child totals.
// Late child calls update the root after its initiating run is terminal.
type UsageStore interface {
	RecordUsage(context.Context, string, string, event.UsageCall) error
	Usage(context.Context, string, string) (event.RunUsage, error)
}

func addUsage(u event.RunUsage, c event.UsageCall, own bool) event.RunUsage {
	if own {
		u.Own = u.Own.AddCall(c)
	} else {
		u.Children = u.Children.AddCall(c)
	}
	u.Total = u.Own.Add(u.Children)
	return u
}
func (m *Memory) RecordUsage(ctx context.Context, tenant, id string, c event.UsageCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.AccountingID == "" {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := m.get(tenant, id)
	if err != nil {
		return err
	}
	own := true
	for n := 0; ; n++ {
		if n > 32 {
			return ErrInvalid
		}
		key := scope(tenant, rec.ID)
		dedupe := key + "\x00" + c.AccountingID
		if !m.usageSeen[dedupe] {
			m.usageSeen[dedupe] = true
			m.usage[key] = addUsage(m.usage[key], c, own)
		}
		if rec.Depth == 0 {
			return nil
		}
		rec, err = m.get(tenant, rec.Key.Parent)
		if err != nil {
			return err
		}
		own = false
	}
}
func (m *Memory) Usage(ctx context.Context, tenant, id string) (event.RunUsage, error) {
	if err := ctx.Err(); err != nil {
		return event.RunUsage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, err := m.get(tenant, id); err != nil {
		return event.RunUsage{}, err
	}
	raw, _ := json.Marshal(m.usage[scope(tenant, id)])
	var out event.RunUsage
	_ = json.Unmarshal(raw, &out)
	return out, nil
}
func (p *Postgres) RecordUsage(ctx context.Context, tenant, id string, c event.UsageCall) error {
	if c.AccountingID == "" {
		return ErrInvalid
	}
	// Lock ancestors root first, so siblings cannot deadlock while rolling up.
	chain := []Record{}
	rec, err := p.Get(ctx, tenant, id)
	if err != nil {
		return err
	}
	for n := 0; ; n++ {
		if n > 32 {
			return ErrInvalid
		}
		chain = append(chain, rec)
		if rec.Depth == 0 {
			break
		}
		rec, err = p.Get(ctx, tenant, rec.Key.Parent)
		if err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for i := len(chain) - 1; i >= 0; i-- {
			id := chain[i].ID
			if _, err := tx.Exec(ctx, `INSERT INTO fiber_session_totals(tenant,session,body) VALUES($1,$2,'{}') ON CONFLICT DO NOTHING`, tenant, id); err != nil {
				return err
			}
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT body FROM fiber_session_totals WHERE tenant=$1 AND session=$2 FOR UPDATE`, tenant, id).Scan(&raw); err != nil {
				return err
			}
			inserted, err := tx.Exec(ctx, `INSERT INTO fiber_session_usage_ids(tenant,session,accounting_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, tenant, id, c.AccountingID)
			if err != nil {
				return err
			}
			if inserted.RowsAffected() == 0 {
				continue
			}
			var u event.RunUsage
			if err := json.Unmarshal(raw, &u); err != nil {
				return fmt.Errorf("session: decode usage: %w", err)
			}
			u = addUsage(u, c, i == 0)
			raw, err = json.Marshal(u)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE fiber_session_totals SET body=$3 WHERE tenant=$1 AND session=$2`, tenant, id, raw); err != nil {
				return err
			}
		}
		return nil
	})
}
func (p *Postgres) Usage(ctx context.Context, tenant, id string) (event.RunUsage, error) {
	if _, err := p.Get(ctx, tenant, id); err != nil {
		return event.RunUsage{}, err
	}
	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT COALESCE((SELECT body FROM fiber_session_totals WHERE tenant=$1 AND session=$2),'{}'::jsonb)`, tenant, id).Scan(&raw)
	if err != nil {
		return event.RunUsage{}, err
	}
	var out event.RunUsage
	err = json.Unmarshal(raw, &out)
	return out, err
}
