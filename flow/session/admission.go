package session

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

var ErrLimit = errors.New("session: open child limit reached")

// Reservation survives a process dying between admission and recording the
// child's run ID. The stable Call key lets that attempt finish on replay.
type Reservation struct {
	Call  string `json:"call"`
	Child string `json:"child"`
	Run   string `json:"run,omitempty"`
}

// AdmissionStore atomically limits pending child calls. A runtime releases a
// reservation only after the ledger proves it terminal or admission failed.
// Implementations serialize Reserve per parent, across processes on shared stores.
type AdmissionStore interface {
	Reserve(context.Context, string, string, string, string, int) error
	BindReservation(context.Context, string, string, string, string) error
	Release(context.Context, string, string, string) error
	Reservations(context.Context, string, string) ([]Reservation, error)
}

func (m *Memory) Reserve(ctx context.Context, tenant, parent, call, child string, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if call == "" || n < 1 || n > 64 {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.get(tenant, parent); err != nil {
		return err
	}
	c, err := m.get(tenant, child)
	if err != nil {
		return err
	}
	if c.Key.Parent != parent {
		return ErrDenied
	}
	key := scope(tenant, parent)
	rows := m.reservations[key]
	for _, r := range rows {
		if r.Call == call {
			if r.Child != child {
				return ErrInvalid
			}
			return nil
		}
	}
	if len(rows) >= n {
		return ErrLimit
	}
	m.reservations[key] = append(rows, Reservation{Call: call, Child: child})
	return nil
}
func (m *Memory) BindReservation(ctx context.Context, tenant, parent, call, run string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == "" {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope(tenant, parent)
	for i, r := range m.reservations[key] {
		if r.Call == call {
			if r.Run != "" && r.Run != run {
				return ErrInvalid
			}
			m.reservations[key][i].Run = run
			return nil
		}
	}
	return ErrNotFound
}
func (m *Memory) Release(ctx context.Context, tenant, parent, call string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope(tenant, parent)
	m.reservations[key] = slices.DeleteFunc(m.reservations[key], func(r Reservation) bool { return r.Call == call })
	return nil
}
func (m *Memory) Reservations(ctx context.Context, tenant, parent string) ([]Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, err := m.get(tenant, parent); err != nil {
		return nil, err
	}
	return append([]Reservation{}, m.reservations[scope(tenant, parent)]...), nil
}
func (p *Postgres) Reserve(ctx context.Context, tenant, parent, call, child string, n int) error {
	if call == "" || n < 1 || n > 64 {
		return ErrInvalid
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM fiber_sessions WHERE tenant=$1 AND id=$2 FOR UPDATE`, tenant, parent).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var actual string
		if err := tx.QueryRow(ctx, `SELECT parent FROM fiber_sessions WHERE tenant=$1 AND id=$2`, tenant, child).Scan(&actual); err != nil {
			return ErrNotFound
		}
		if actual != parent {
			return ErrDenied
		}
		var existing string
		err := tx.QueryRow(ctx, `SELECT child FROM fiber_session_reservations WHERE tenant=$1 AND parent=$2 AND call=$3`, tenant, parent, call).Scan(&existing)
		if err == nil {
			if existing != child {
				return ErrInvalid
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM fiber_session_reservations WHERE tenant=$1 AND parent=$2`, tenant, parent).Scan(&count); err != nil {
			return err
		}
		if count >= n {
			return ErrLimit
		}
		_, err = tx.Exec(ctx, `INSERT INTO fiber_session_reservations(tenant,parent,call,child,run) VALUES($1,$2,$3,$4,'')`, tenant, parent, call, child)
		return err
	})
}
func (p *Postgres) BindReservation(ctx context.Context, tenant, parent, call, run string) error {
	if run == "" {
		return ErrInvalid
	}
	tag, err := p.pool.Exec(ctx, `UPDATE fiber_session_reservations SET run=$4 WHERE tenant=$1 AND parent=$2 AND call=$3 AND (run='' OR run=$4)`, tenant, parent, call, run)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: reservation missing or bound to another run", ErrInvalid)
	}
	return nil
}
func (p *Postgres) Release(ctx context.Context, tenant, parent, call string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM fiber_session_reservations WHERE tenant=$1 AND parent=$2 AND call=$3`, tenant, parent, call)
	return err
}
func (p *Postgres) Reservations(ctx context.Context, tenant, parent string) ([]Reservation, error) {
	if _, err := p.Get(ctx, tenant, parent); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT call,child,run FROM fiber_session_reservations WHERE tenant=$1 AND parent=$2 ORDER BY call LIMIT 64`, tenant, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reservation{}
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.Call, &r.Child, &r.Run); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
