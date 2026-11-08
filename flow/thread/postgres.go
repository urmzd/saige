package thread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/internal/legacy"
)

// schema is everything the Postgres store owns: one row per thread, one row
// per entry.
//
// fiber_threads holds the single fact about a thread that changes, its
// head, and exists to be locked: an append takes the row FOR UPDATE, so two
// appends to one thread serialize on it and two threads never wait for each
// other. head is kept there rather than derived with max(seq) so that the
// number an append assigns and the number a run pins come from one place.
//
// fiber_thread_entries holds what does not change. This package never
// issues an UPDATE or a DELETE against it.
//
// attachments was added after the table first shipped, so it arrives by an
// ALTER that is safe to run on every start: IF NOT EXISTS makes it a no-op
// once applied, and the constant default means a row written before it
// existed reads back as an entry with no attachments, without a rewrite of
// the table. The column holds references (digest, type, name), never bytes.
//
// The two partial unique indexes are the idempotency rule written as
// constraints: at most one entry per (run, role), at most one per surface
// message. Append checks both before it inserts, under the thread's lock, so
// the indexes are never what stops a duplicate. They are there so that a
// writer who bypassed the lock would fail instead of corrupting the journal.
const schema = `
CREATE TABLE IF NOT EXISTS fiber_threads (
	tenant  text   NOT NULL,
	agent   text   NOT NULL,
	surface text   NOT NULL,
	head    bigint NOT NULL CHECK (head >= 0),
	PRIMARY KEY (tenant, agent, surface)
);

CREATE TABLE IF NOT EXISTS fiber_thread_entries (
	tenant        text        NOT NULL,
	agent         text        NOT NULL,
	surface       text        NOT NULL,
	seq           bigint      NOT NULL CHECK (seq >= 1),
	role          text        NOT NULL CHECK (role IN ('user', 'assistant')),
	body          text        NOT NULL,
	actor_id      text        NOT NULL,
	actor_display text        NOT NULL,
	actor_bot     boolean     NOT NULL,
	run_id        text        NOT NULL,
	surface_id    text        NOT NULL,
	at            timestamptz NOT NULL,
	PRIMARY KEY (tenant, agent, surface, seq),
	FOREIGN KEY (tenant, agent, surface) REFERENCES fiber_threads (tenant, agent, surface)
);

CREATE UNIQUE INDEX IF NOT EXISTS fiber_thread_entries_run
	ON fiber_thread_entries (tenant, agent, surface, run_id, role)
	WHERE run_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS fiber_thread_entries_surface
	ON fiber_thread_entries (tenant, agent, surface, surface_id)
	WHERE surface_id <> '';

ALTER TABLE fiber_thread_entries
	ADD COLUMN IF NOT EXISTS attachments jsonb NOT NULL DEFAULT '[]'::jsonb;
`

// migrateLock is the advisory lock Migrate holds, the same one every flow
// store takes. CREATE TABLE IF NOT EXISTS is not safe against itself: two
// processes starting together can both pass the existence check and one then
// fails on the catalog's unique index.
const migrateLock = 0x6d616e64 // "mand"

// Postgres is a Store that outlives the process.
//
// It keeps every rule Memory documents and is held to them by the same tests:
// the suite in this package runs against Postgres whenever
// $SAIGE_TEST_POSTGRES_DSN is set.
//
// It does not own the pool. The caller opened it and the caller closes it,
// because a deployment that already has a pool should not be made to hold two.
type Postgres struct {
	pool *pgxpool.Pool
	settings
}

// NewPostgres returns a store on pool. It does not touch the database; call
// Migrate once before the first use.
func NewPostgres(pool *pgxpool.Pool, opts ...Option) *Postgres {
	return &Postgres{pool: pool, settings: newSettings(opts)}
}

var _ Store = (*Postgres)(nil)

// Migrate creates the tables this store needs if they are not there. It is
// idempotent and safe to run from every replica at startup.
func (p *Postgres) Migrate(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLock)); err != nil {
			return err
		}
		// A database created before the rename is moved forward first.
		if _, err := tx.Exec(ctx, legacy.FiberTablesSQL("threads", "thread_entries")); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		return fmt.Errorf("thread: migrate: %w", err)
	}
	return nil
}

const entryColumns = `seq, role, body, actor_id, actor_display, actor_bot, run_id, surface_id, at, attachments`

// Append adds entries to the end of a thread.
//
// The thread's row is claimed by inserting it before reading it, then locked.
// Two first appends to a new thread then meet on the primary key rather than
// on a check each could pass, and every later append waits its turn on the
// row lock. Everything after that, the duplicate check included, happens
// while holding it, which is what makes the call atomic, contiguous and
// idempotent at once.
func (p *Postgres) Append(ctx context.Context, key Key, entries ...Entry) ([]Entry, error) {
	prepared, err := p.prepare(ctx, key, entries)
	if err != nil {
		return nil, err
	}
	if len(prepared) == 0 {
		return []Entry{}, nil
	}
	runs := make([]string, 0, len(prepared))
	surfaces := make([]string, 0, len(prepared))
	for _, e := range prepared {
		if e.RunID != "" {
			runs = append(runs, e.RunID)
		}
		if e.SurfaceID != "" {
			surfaces = append(surfaces, e.SurfaceID)
		}
	}

	out := make([]Entry, len(prepared))
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO fiber_threads (tenant, agent, surface, head) VALUES ($1, $2, $3, 0)
			 ON CONFLICT (tenant, agent, surface) DO NOTHING`,
			key.Tenant, key.Agent, key.Surface); err != nil {
			return err
		}
		var head int64
		if err := tx.QueryRow(ctx,
			`SELECT head FROM fiber_threads
			  WHERE tenant = $1 AND agent = $2 AND surface = $3 FOR UPDATE`,
			key.Tenant, key.Agent, key.Surface).Scan(&head); err != nil {
			return err
		}

		// Only the entries this call could duplicate are read, not the whole
		// thread: a reconciliation of a long thread names every message on
		// the surface, and nearly all of them are already here.
		rows, err := tx.Query(ctx,
			`SELECT `+entryColumns+` FROM fiber_thread_entries
			  WHERE tenant = $1 AND agent = $2 AND surface = $3
			    AND ((run_id <> '' AND run_id = ANY($4)) OR (surface_id <> '' AND surface_id = ANY($5)))
			  ORDER BY seq`,
			key.Tenant, key.Agent, key.Surface, runs, surfaces)
		if err != nil {
			return err
		}
		held, err := collect(rows)
		if err != nil {
			return err
		}
		j := &journal{entries: held, byRun: make(map[runRole]int), bySurface: make(map[string]int)}
		for i, e := range held {
			j.index(e, i)
		}

		batch := &pgx.Batch{}
		for i, e := range prepared {
			if at, ok := j.held(e); ok {
				out[i] = j.entries[at]
				continue
			}
			head++
			e.Seq = uint64(head) //nolint:gosec // sequence numbers are BIGINT and never negative
			j.entries = append(j.entries, e)
			j.index(e, len(j.entries)-1)
			out[i] = e
			files, err := encodeAttachments(e.Attachments)
			if err != nil {
				return err
			}
			batch.Queue(
				`INSERT INTO fiber_thread_entries
				   (tenant, agent, surface, seq, role, body, actor_id, actor_display, actor_bot, run_id, surface_id, at, attachments)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				key.Tenant, key.Agent, key.Surface, head, string(e.Role), e.Text,
				e.Actor.ID, e.Actor.Display, e.Actor.Bot, e.RunID, e.SurfaceID, e.At, files)
		}
		if batch.Len() == 0 {
			return nil
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE fiber_threads SET head = $4 WHERE tenant = $1 AND agent = $2 AND surface = $3`,
			key.Tenant, key.Agent, key.Surface, head)
		return err
	})
	if err != nil {
		return nil, wrap("append", err)
	}
	return out, nil
}

// Head returns the sequence of a thread's newest entry.
func (p *Postgres) Head(ctx context.Context, key Key) (uint64, error) {
	if err := check(ctx, key); err != nil {
		return 0, err
	}
	var head int64
	err := p.pool.QueryRow(ctx,
		`SELECT head FROM fiber_threads WHERE tenant = $1 AND agent = $2 AND surface = $3`,
		key.Tenant, key.Agent, key.Surface).Scan(&head)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, wrap("head", err)
	}
	return uint64(head), nil //nolint:gosec // sequence numbers are BIGINT and never negative
}

// History returns the entries with Seq <= upTo, oldest first.
//
// It reads the entries and not the head: the head moves, and the answer for a
// given upTo must not.
func (p *Postgres) History(ctx context.Context, key Key, upTo uint64) ([]Entry, error) {
	if err := check(ctx, key); err != nil {
		return nil, err
	}
	if upTo == 0 {
		return []Entry{}, nil
	}
	if upTo > math.MaxInt64 {
		return nil, fmt.Errorf("%w: thread %s cannot hold %d entries", ErrGap, key, upTo)
	}
	rows, err := p.pool.Query(ctx,
		`SELECT `+entryColumns+` FROM fiber_thread_entries
		  WHERE tenant = $1 AND agent = $2 AND surface = $3 AND seq <= $4
		  ORDER BY seq`,
		key.Tenant, key.Agent, key.Surface, int64(upTo))
	if err != nil {
		return nil, wrap("history", err)
	}
	out, err := collect(rows)
	if err != nil {
		return nil, wrap("history", err)
	}
	// Sequences are unique and start at 1, so holding upTo of them at or
	// below upTo is the same as holding every one.
	if uint64(len(out)) != upTo {
		return nil, fmt.Errorf("%w: thread %s holds %d entries, not the %d asked for", ErrGap, key, len(out), upTo)
	}
	return out, nil
}

// collect decodes rows of entryColumns.
func collect(rows pgx.Rows) ([]Entry, error) {
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var (
			e     Entry
			seq   int64
			role  string
			a     event.Actor
			files []byte
		)
		if err := rows.Scan(&seq, &role, &e.Text, &a.ID, &a.Display, &a.Bot, &e.RunID, &e.SurfaceID, &e.At, &files); err != nil {
			return nil, err
		}
		e.Seq, e.Role, e.Actor, e.At = uint64(seq), Role(role), a, e.At.UTC() //nolint:gosec // sequence numbers are BIGINT and never negative
		if err := json.Unmarshal(files, &e.Attachments); err != nil {
			return nil, fmt.Errorf("decode attachments of entry %d: %w", seq, err)
		}
		out = append(out, e.clone())
	}
	return out, rows.Err()
}

// encodeAttachments renders an entry's attachment list for its column. No
// attachments is the empty array the column defaults to, so an entry written
// now with none is stored exactly as a row written before the column existed.
func encodeAttachments(as []attachment.Attachment) ([]byte, error) {
	if len(as) == 0 {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(as)
	if err != nil {
		return nil, fmt.Errorf("encode attachments: %w", err)
	}
	return b, nil
}

// wrap adds this package's context to a database failure and leaves the
// sentinels alone, so a caller matching ErrGap reads what is missing rather
// than "postgres: history".
func wrap(op string, err error) error {
	for _, sentinel := range []error{ErrInvalid, ErrGap, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("thread: postgres %s: %w", op, err)
}
