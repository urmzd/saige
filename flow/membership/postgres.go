package membership

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schema is the one table the Postgres store owns.
const schema = `
CREATE TABLE IF NOT EXISTS fiber_memberships (
	tenant    text        NOT NULL,
	channel   text        NOT NULL,
	agent     text        NOT NULL,
	rule      jsonb       NOT NULL,
	joined_at timestamptz NOT NULL,
	PRIMARY KEY (tenant, channel, agent)
);
`

// migrateLock is the advisory lock every flow store takes to migrate.
const migrateLock = 0x6d616e64 // "mand"

// Postgres is a Store shared by every process on one database. It does not
// own the pool.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a store on pool. Call Migrate once before first use.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

var _ Store = (*Postgres)(nil)

// Migrate creates the table if it is not there. It is idempotent.
func (p *Postgres) Migrate(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLock)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		return fmt.Errorf("membership: migrate: %w", err)
	}
	return nil
}

// Join implements Store.
func (p *Postgres) Join(ctx context.Context, tenant, channel, agent string, rule Rule) error {
	if err := validate(tenant, channel, agent); err != nil {
		return err
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return fmt.Errorf("membership: encode rule: %w", err)
	}
	_, err = p.pool.Exec(ctx,
		`INSERT INTO fiber_memberships (tenant, channel, agent, rule, joined_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant, channel, agent) DO UPDATE SET rule = EXCLUDED.rule`,
		tenant, channel, agent, raw, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("membership: join: %w", err)
	}
	return nil
}

// Leave implements Store.
func (p *Postgres) Leave(ctx context.Context, tenant, channel, agent string) error {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM fiber_memberships WHERE tenant = $1 AND channel = $2 AND agent = $3`,
		tenant, channel, agent)
	if err != nil {
		return fmt.Errorf("membership: leave: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Members implements Store. It reads only the rows whose channel is one of
// the prefixes that can select channel, so the query is an index lookup.
func (p *Postgres) Members(ctx context.Context, tenant, channel string) ([]Member, error) {
	if tenant == "" || channel == "" {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx,
		`SELECT tenant, channel, agent, rule, joined_at FROM fiber_memberships
		  WHERE tenant = $1 AND channel = ANY($2)
		  ORDER BY agent, channel
		  LIMIT $3`,
		tenant, prefixes(channel), MaxMembers*4)
	if err != nil {
		return nil, fmt.Errorf("membership: members: %w", err)
	}
	defer rows.Close()
	var all []Member
	for rows.Next() {
		var (
			m   Member
			raw []byte
		)
		if err := rows.Scan(&m.Tenant, &m.Channel, &m.Agent, &raw, &m.Joined); err != nil {
			return nil, fmt.Errorf("membership: members: %w", err)
		}
		if err := json.Unmarshal(raw, &m.Rule); err != nil {
			return nil, fmt.Errorf("membership: members: rule of %s on %s: %w", m.Agent, m.Channel, err)
		}
		m.Joined = m.Joined.UTC()
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("membership: members: %w", err)
	}
	return pick(channel, all), nil
}
