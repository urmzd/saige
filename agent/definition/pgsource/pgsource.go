// Package pgsource keeps agent definitions in PostgreSQL: one row per
// name and version in saige_agent_definitions, holding the file as
// written and its digest. postgres.RunMigrations creates the table and a
// trigger that announces every change on Channel, so a registry watching
// through a postgres.Notifier reloads when any process, or plain SQL,
// changes a row.
package pgsource

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/types"
)

// Channel is the notification channel the table's trigger publishes on.
const Channel = "saige_agent_definitions"

// Source is a definition.Source and definition.Watcher over the
// saige_agent_definitions table.
type Source struct {
	pool     *pgxpool.Pool
	notifier types.Notifier
}

// New returns a source over pool. notifier delivers change notifications
// for Watch, normally a postgres.Notifier on the same database. With nil,
// Watch never fires and a registry relies on its polling interval.
func New(pool *pgxpool.Pool, notifier types.Notifier) *Source {
	return &Source{pool: pool, notifier: notifier}
}

func (s *Source) String() string { return "postgres:saige_agent_definitions" }

// Load reads every row and parses it. A row whose body does not parse,
// does not match its name and version, or does not match its digest fails
// the load, since it was changed outside Put.
func (s *Source) Load(ctx context.Context) ([]*definition.Definition, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, version, body, digest FROM saige_agent_definitions ORDER BY name, version`)
	if err != nil {
		return nil, fmt.Errorf("agent definitions: query: %w", err)
	}
	defer rows.Close()
	var defs []*definition.Definition
	var errs []error
	for rows.Next() {
		var name, version, body, digest string
		if err := rows.Scan(&name, &version, &body, &digest); err != nil {
			return nil, fmt.Errorf("agent definitions: scan: %w", err)
		}
		ref := name + "@" + version
		d, err := definition.Parse([]byte(body), ref)
		switch {
		case err != nil:
			errs = append(errs, err)
			continue
		case d.Name != name || d.Version != version:
			errs = append(errs, fmt.Errorf("agent definitions: row %s holds %s", ref, d.ID()))
			continue
		case d.Digest != digest:
			errs = append(errs, fmt.Errorf("agent definitions: row %s does not match its digest", ref))
			continue
		}
		d.Source = s.String()
		defs = append(defs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent definitions: %w", err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return defs, nil
}

// Put parses body and stores it under its name and version, replacing the
// row that was there. An invalid definition is rejected before anything is
// written.
func (s *Source) Put(ctx context.Context, body []byte) (*definition.Definition, error) {
	d, err := definition.Parse(body, "put")
	if err != nil {
		return nil, err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO saige_agent_definitions (name, version, body, digest, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (name, version) DO UPDATE
		 SET body = EXCLUDED.body, digest = EXCLUDED.digest, updated_at = now()`,
		d.Name, d.Version, string(d.Raw()), d.Digest)
	if err != nil {
		return nil, fmt.Errorf("agent definitions: put %s: %w", d.ID(), err)
	}
	d.Source = s.String()
	return d, nil
}

// Delete removes one version. It reports whether a row was removed.
func (s *Source) Delete(ctx context.Context, name, version string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM saige_agent_definitions WHERE name = $1 AND version = $2`, name, version)
	if err != nil {
		return false, fmt.Errorf("agent definitions: delete %s@%s: %w", name, version, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Watch calls fn after each change to the table, as announced on Channel.
func (s *Source) Watch(ctx context.Context, fn func()) (func(), error) {
	if s.notifier == nil {
		return func() {}, nil
	}
	return notify.Listen(ctx, s.notifier, Channel, func(context.Context, types.Notification) { fn() })
}
