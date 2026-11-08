package definition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schema is everything the Postgres source owns. fiber_definitions holds
// one row per version and is only ever inserted into: this package issues no
// UPDATE and no DELETE against it. A retirement is a row of its own.
//
// name and version are COLLATE "C" so List orders by the same comparison
// Memory uses, and a page token means the same position in both.
const schema = `
CREATE TABLE IF NOT EXISTS fiber_definitions (
	tenant      text        NOT NULL,
	name        text        COLLATE "C" NOT NULL,
	version     text        COLLATE "C" NOT NULL,
	content     text        NOT NULL,
	digest      text        NOT NULL,
	policy      jsonb       NOT NULL,
	policy_hash text        NOT NULL,
	created_at  timestamptz NOT NULL,
	created_by  text        NOT NULL,
	PRIMARY KEY (tenant, name, version)
);

CREATE TABLE IF NOT EXISTS fiber_definition_retirements (
	tenant     text        NOT NULL,
	name       text        COLLATE "C" NOT NULL,
	retired_at timestamptz NOT NULL,
	retired_by text        NOT NULL,
	PRIMARY KEY (tenant, name)
);
`

// migrateLock is the advisory lock Migrate holds, so replicas starting
// together do not race on CREATE TABLE IF NOT EXISTS.
const migrateLock = 0x66646566 // "fdef"

// Postgres is a Source that outlives the process. It does not own the pool.
type Postgres struct {
	settings
	pool *pgxpool.Pool
}

// NewPostgres returns a source on pool. Call Migrate once before first use.
func NewPostgres(pool *pgxpool.Pool, opts ...Option) *Postgres {
	return &Postgres{settings: newSettings(opts), pool: pool}
}

var _ Source = (*Postgres)(nil)

// Migrate creates the tables if they are not there. It is idempotent and
// safe to run from every replica.
func (p *Postgres) Migrate(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLock)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		return fmt.Errorf("definition: migrate: %w", err)
	}
	return nil
}

const columns = `tenant, name, version, content, digest, policy, policy_hash, created_at, created_by`

func scan(row pgx.Row) (Record, error) {
	var (
		r      Record
		policy []byte
		at     time.Time
	)
	if err := row.Scan(&r.Tenant, &r.Name, &r.Version, &r.Content, &r.Digest, &policy, &r.PolicyHash, &at, &r.CreatedBy); err != nil {
		return Record{}, err
	}
	if err := json.Unmarshal(policy, &r.Policy); err != nil {
		return Record{}, fmt.Errorf("definition: %s@%s: decode policy: %w", r.Name, r.Version, err)
	}
	r.Policy = r.Policy.Normalize()
	r.CreatedAt = at.UTC()
	return r, nil
}

// nameLock serializes writes to one name: Put and Retire take it, so a
// version cannot land on a name retired at the same moment.
const nameLock = `SELECT pg_advisory_xact_lock(hashtextextended($1 || '/' || $2, 0))`

// Put implements Source.
func (p *Postgres) Put(ctx context.Context, rec Record) (Record, error) {
	rec, err := p.prepare(rec)
	if err != nil {
		return Record{}, err
	}
	policy, err := json.Marshal(rec.Policy)
	if err != nil {
		return Record{}, fmt.Errorf("definition: encode policy: %w", err)
	}
	var out Record
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, nameLock, rec.Tenant, rec.Name); err != nil {
			return err
		}
		have, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM fiber_definitions WHERE tenant = $1 AND name = $2 AND version = $3`,
			rec.Tenant, rec.Name, rec.Version))
		switch {
		case err == nil:
			if have.same(rec) {
				out = have
				return nil
			}
			return fmt.Errorf("%w: %s@%s holds other content; a version is never rewritten", ErrExists, rec.Name, rec.Version)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var retired bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM fiber_definition_retirements WHERE tenant = $1 AND name = $2)`,
			rec.Tenant, rec.Name).Scan(&retired); err != nil {
			return err
		}
		if retired {
			return fmt.Errorf("%w: %s", ErrRetired, rec.Name)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO fiber_definitions (`+columns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant, name, version) DO NOTHING`,
			rec.Tenant, rec.Name, rec.Version, rec.Content, rec.Digest, policy, rec.PolicyHash, rec.CreatedAt, rec.CreatedBy)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Another writer inserted this version first.
			have, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM fiber_definitions WHERE tenant = $1 AND name = $2 AND version = $3`,
				rec.Tenant, rec.Name, rec.Version))
			if err != nil {
				return err
			}
			if !have.same(rec) {
				return fmt.Errorf("%w: %s@%s holds other content; a version is never rewritten", ErrExists, rec.Name, rec.Version)
			}
			out = have
			return nil
		}
		out = rec
		return nil
	})
	if err != nil {
		return Record{}, wrap("put", err)
	}
	return out, nil
}

// Get implements Source.
func (p *Postgres) Get(ctx context.Context, tenant, name, version string) (Record, error) {
	r, err := scan(p.pool.QueryRow(ctx, `SELECT `+columns+` FROM fiber_definitions WHERE tenant = $1 AND name = $2 AND version = $3`,
		tenant, name, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
	}
	if err != nil {
		return Record{}, wrap("get", err)
	}
	return r, nil
}

// Versions implements Source.
func (p *Postgres) Versions(ctx context.Context, tenant, name string) ([]Record, error) {
	out, err := p.query(ctx, `SELECT `+columns+` FROM fiber_definitions WHERE tenant = $1 AND name = $2`, tenant, name)
	if err != nil {
		return nil, wrap("versions", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	sortVersions(out)
	return out, nil
}

// Latest implements Source.
func (p *Postgres) Latest(ctx context.Context, tenant, name string) (Record, error) {
	var retired bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM fiber_definition_retirements WHERE tenant = $1 AND name = $2)`,
		tenant, name).Scan(&retired); err != nil {
		return Record{}, wrap("latest", err)
	}
	all, err := p.Versions(ctx, tenant, name)
	if err != nil {
		return Record{}, err
	}
	if retired {
		return Record{}, fmt.Errorf("%w: %s", ErrRetired, name)
	}
	return all[len(all)-1], nil
}

// List implements Source. It reads one page of live names, then every
// version of those names in one query.
func (p *Postgres) List(ctx context.Context, tenant string, page Page) ([]Record, string, error) {
	after, err := decodeToken(page.Token, tenant)
	if err != nil {
		return nil, "", err
	}
	size := page.size()
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT d.name FROM fiber_definitions d
		WHERE d.tenant = $1 AND d.name > $2
		  AND NOT EXISTS (SELECT 1 FROM fiber_definition_retirements r WHERE r.tenant = d.tenant AND r.name = d.name)
		ORDER BY d.name LIMIT $3`, tenant, after, size+1)
	if err != nil {
		return nil, "", wrap("list", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, "", wrap("list", err)
	}
	next := ""
	if len(names) > size {
		names = names[:size]
		next = encodeToken(tenant, names[size-1])
	}
	if len(names) == 0 {
		return nil, "", nil
	}
	all, err := p.query(ctx, `SELECT `+columns+` FROM fiber_definitions WHERE tenant = $1 AND name = ANY($2)`, tenant, names)
	if err != nil {
		return nil, "", wrap("list", err)
	}
	latest := map[string]Record{}
	for _, r := range all {
		if have, ok := latest[r.Name]; !ok || byVersion(have, r) < 0 {
			latest[r.Name] = r
		}
	}
	out := make([]Record, 0, len(names))
	for _, n := range names {
		out = append(out, latest[n])
	}
	return out, next, nil
}

// Retire implements Source. Retiring twice keeps the first retirement.
func (p *Postgres) Retire(ctx context.Context, tenant, name, by string) error {
	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("%w: a retirement must say who made it", ErrInvalid)
	}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, nameLock, tenant, name); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fiber_definitions WHERE tenant = $1 AND name = $2)`,
			tenant, name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		_, err := tx.Exec(ctx, `INSERT INTO fiber_definition_retirements (tenant, name, retired_at, retired_by)
			VALUES ($1, $2, $3, $4) ON CONFLICT (tenant, name) DO NOTHING`,
			tenant, name, p.clock().UTC().Truncate(time.Microsecond), strings.TrimSpace(by))
		return err
	})
	if err != nil {
		return wrap("retire", err)
	}
	return nil
}

func (p *Postgres) query(ctx context.Context, sql string, args ...any) ([]Record, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// wrap names the operation on a store failure and leaves this package's own
// errors as they are.
func wrap(op string, err error) error {
	for _, own := range []error{ErrNotFound, ErrExists, ErrConflict, ErrInvalid, ErrRetired} {
		if errors.Is(err, own) {
			return err
		}
	}
	return fmt.Errorf("definition: %s: %w", op, err)
}
