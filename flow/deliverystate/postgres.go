package deliverystate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `CREATE TABLE IF NOT EXISTS fiber_sink_state (
 tenant text NOT NULL,
 key text NOT NULL,
 scope text NOT NULL,
 identity jsonb NOT NULL,
 pending boolean NOT NULL DEFAULT false,
 state bytea NOT NULL DEFAULT ''::bytea,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant, key)
);
CREATE INDEX IF NOT EXISTS fiber_sink_state_pending ON fiber_sink_state (scope,updated_at) WHERE pending;`

// Postgres shares sink state and per-message locks across replicas. It does not
// own the pool. Records are retained until the host removes them; removing a
// record also removes its duplicate-delivery protection.
type Postgres struct{ pool *pgxpool.Pool }

// NewPostgres returns a store on pool. Call Migrate before use.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Migrate creates the table under the common flow migration lock.
func (p *Postgres) Migrate(ctx context.Context) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x6d616e64)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
}

// Update holds the row lock across the callback, preventing two replicas from
// creating or editing the same message concurrently. The caller's context must
// bound external API calls so a stalled surface cannot hold a lock indefinitely.
func (p *Postgres) Update(ctx context.Context, k Key, update func([]byte) ([]byte, bool, error)) error {
	id, err := k.id()
	if err != nil {
		return err
	}
	identity, err := json.Marshal(k)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO fiber_sink_state (tenant,key,scope,identity) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, k.Tenant, id, k.Scope, identity); err != nil {
			return err
		}
		var data []byte
		if err := tx.QueryRow(ctx, `SELECT state FROM fiber_sink_state WHERE tenant=$1 AND key=$2 FOR UPDATE`, k.Tenant, id).Scan(&data); err != nil {
			return err
		}
		data, pending, err := update(data)
		if err != nil {
			return err
		}
		if data == nil {
			data = []byte{}
		}
		_, err = tx.Exec(ctx, `UPDATE fiber_sink_state SET state=$3,pending=$4, updated_at=now() WHERE tenant=$1 AND key=$2`, k.Tenant, id, data, pending)
		return err
	})
	if err != nil {
		return fmt.Errorf("deliverystate: update: %w", err)
	}
	return nil
}

// Pending implements Store with a bounded indexed query.
func (p *Postgres) Pending(ctx context.Context, scope string, limit int) ([]Key, error) {
	if limit <= 0 || limit > 256 {
		return nil, fmt.Errorf("deliverystate: pending limit must be 1..256")
	}
	rows, err := p.pool.Query(ctx, `SELECT identity FROM fiber_sink_state WHERE scope=$1 AND pending ORDER BY updated_at,key LIMIT $2`, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var k Key
		if err := json.Unmarshal(raw, &k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
