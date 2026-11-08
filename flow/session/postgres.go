package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists identities and links on the host's pool, which it never closes.
type Postgres struct{ pool *pgxpool.Pool }

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }
func (p *Postgres) Migrate(ctx context.Context) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x6d616e64)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
CREATE TABLE IF NOT EXISTS fiber_sessions (
 tenant text NOT NULL, id text NOT NULL, parent text NOT NULL, agent text NOT NULL,
 label text NOT NULL, channel text NOT NULL, depth integer NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant,id), UNIQUE(tenant,parent,agent,label)
);
CREATE TABLE IF NOT EXISTS fiber_session_links (
 tenant text NOT NULL, session text NOT NULL, target text NOT NULL, kind text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(tenant,session,target,kind),
 FOREIGN KEY(tenant,session) REFERENCES fiber_sessions(tenant,id),
 FOREIGN KEY(tenant,target) REFERENCES fiber_sessions(tenant,id)
);
CREATE TABLE IF NOT EXISTS fiber_session_runs (
 tenant text NOT NULL, session text NOT NULL, run text NOT NULL, position bigserial,
 PRIMARY KEY(tenant,session,run), FOREIGN KEY(tenant,session) REFERENCES fiber_sessions(tenant,id)
);
CREATE TABLE IF NOT EXISTS fiber_session_totals (tenant text NOT NULL, session text NOT NULL, body jsonb NOT NULL, PRIMARY KEY(tenant,session));
CREATE TABLE IF NOT EXISTS fiber_session_usage_ids (tenant text NOT NULL, session text NOT NULL, accounting_id text NOT NULL, PRIMARY KEY(tenant,session,accounting_id));
CREATE TABLE IF NOT EXISTS fiber_session_budgets (tenant text NOT NULL, account text NOT NULL, body jsonb NOT NULL, PRIMARY KEY(tenant,account));
CREATE TABLE IF NOT EXISTS fiber_session_grants (
 tenant text NOT NULL, target text NOT NULL, reader text NOT NULL, PRIMARY KEY(tenant,target,reader),
 FOREIGN KEY(tenant,target) REFERENCES fiber_sessions(tenant,id), FOREIGN KEY(tenant,reader) REFERENCES fiber_sessions(tenant,id)
);
CREATE TABLE IF NOT EXISTS fiber_session_reservations (
 tenant text NOT NULL, parent text NOT NULL, call text NOT NULL, child text NOT NULL, run text NOT NULL,
 PRIMARY KEY(tenant,parent,call), FOREIGN KEY(tenant,parent) REFERENCES fiber_sessions(tenant,id),
 FOREIGN KEY(tenant,child) REFERENCES fiber_sessions(tenant,id)
);
CREATE INDEX IF NOT EXISTS fiber_sessions_parent ON fiber_sessions(tenant,parent,created_at,id);
`)
		return err
	})
}

const columns = `id,tenant,parent,agent,label,channel,depth,created_at`

func scan(row pgx.Row) (Record, error) {
	var r Record
	err := row.Scan(&r.ID, &r.Key.Tenant, &r.Key.Parent, &r.Key.Agent, &r.Key.Label, &r.Channel, &r.Depth, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}
func (p *Postgres) Open(ctx context.Context, tenant string, o Open) (Record, error) {
	k, err := prepare(tenant, o)
	if err != nil {
		return Record{}, err
	}
	depth := 0
	if strings.HasPrefix(k.Parent, "s_") {
		parent, err := p.Get(ctx, tenant, k.Parent)
		if err != nil {
			return Record{}, err
		}
		depth = parent.Depth + 1
	}
	// The unique key is the arbiter across replicas. Conflict updates no identity.
	return scan(p.pool.QueryRow(ctx, `INSERT INTO fiber_sessions(tenant,id,parent,agent,label,channel,depth)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(tenant,id) DO UPDATE SET id=EXCLUDED.id RETURNING `+columns,
		tenant, k.ID(), k.Parent, k.Agent, k.Label, channel(k, o.Channel), depth))
}
func (p *Postgres) Get(ctx context.Context, tenant, id string) (Record, error) {
	if tenant == "" {
		return Record{}, ErrNotFound
	}
	return scan(p.pool.QueryRow(ctx, `SELECT `+columns+` FROM fiber_sessions WHERE tenant=$1 AND id=$2`, tenant, id))
}
func (p *Postgres) List(ctx context.Context, tenant string, f Filter) ([]Record, error) {
	if tenant == "" {
		return nil, ErrInvalid
	}
	rows, err := p.pool.Query(ctx, `SELECT `+columns+` FROM fiber_sessions WHERE tenant=$1 AND ($2='' OR parent=$2) AND ($3='' OR agent=$3) ORDER BY created_at,id LIMIT $4`, tenant, f.Parent, f.Agent, limit(f.Limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (p *Postgres) Link(ctx context.Context, tenant, id string, l Link) error {
	if !validLink(l) || l.Target == id {
		return ErrInvalid
	}
	if _, err := p.Get(ctx, tenant, id); err != nil {
		return err
	}
	if _, err := p.Get(ctx, tenant, l.Target); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO fiber_session_links(tenant,session,target,kind) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, tenant, id, l.Target, l.Kind)
	return err
}
func (p *Postgres) Links(ctx context.Context, tenant, id string, n int) ([]Link, error) {
	if _, err := p.Get(ctx, tenant, id); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT target,kind,created_at FROM fiber_session_links WHERE tenant=$1 AND session=$2 ORDER BY created_at,target,kind LIMIT $3`, tenant, id, limit(n))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Target, &l.Kind, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (p *Postgres) BindRun(ctx context.Context, tenant, id, run string) error {
	if run == "" {
		return ErrInvalid
	}
	if _, err := p.Get(ctx, tenant, id); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO fiber_session_runs(tenant,session,run) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, tenant, id, run)
	if err != nil {
		return fmt.Errorf("session: bind run: %w", err)
	}
	return nil
}
func (p *Postgres) Runs(ctx context.Context, tenant, id string, n int) ([]string, error) {
	if _, err := p.Get(ctx, tenant, id); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT run FROM (SELECT run,position FROM fiber_session_runs WHERE tenant=$1 AND session=$2 ORDER BY position DESC LIMIT $3) recent ORDER BY position`, tenant, id, limit(n))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
