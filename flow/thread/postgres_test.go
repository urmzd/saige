package thread_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/thread"
)

// postgresEnv names the Postgres the Store suite runs against. Without it the
// suite runs against Memory, so it stays runnable with no infrastructure; with
// it, the Postgres backend is held to exactly the same contract.
const postgresEnv = "SAIGE_TEST_POSTGRES_DSN"

// schemas numbers each store the suite builds, so tests sharing one database
// cannot see each other's threads.
var schemas atomic.Uint64

// newPostgres returns a migrated store in a schema of its own, dropped when the
// test ends. The pool is capped well under Postgres's default connection limit
// because the package's tests run in parallel with every other package's.
func newPostgres(t *testing.T, url string, opts ...thread.Option) *thread.Postgres {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_thread_%d_%d", time.Now().UnixNano(), schemas.Add(1))

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse $%s: %v", postgresEnv, err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		pool.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
		pool.Close()
	})

	store := thread.NewPostgres(pool, opts...)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pools.Store(store, pool)
	return store
}

// pools remembers the pool behind each store the suite built, for the tests
// that need to reach around the Store interface.
var pools sync.Map

func poolOf(t *testing.T, store *thread.Postgres) *pgxpool.Pool {
	t.Helper()
	pool, ok := pools.Load(store)
	if !ok {
		t.Fatal("store was not built by newPostgres")
	}
	return pool.(*pgxpool.Pool)
}

// Migrate runs from every replica at startup, so running it again, and running
// it from several places at once, has to be a no-op rather than an error.
func TestPostgresMigrateIsIdempotentAndConcurrent(t *testing.T) {
	url := postgresURL(t)
	store := newPostgres(t, url)

	errs := make(chan error, 8)
	for range cap(errs) {
		go func() { errs <- store.Migrate(context.Background()) }()
	}
	for range cap(errs) {
		if err := <-errs; err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
}

// The reason this backend exists: a second store on the same database, which
// is what a restarted process is, reads the conversation the first one wrote
// and continues its numbering.
func TestPostgresConversationsSurviveANewStore(t *testing.T) {
	url := postgresURL(t)
	first := newPostgres(t, url)
	key := slackThread("docs-bot")
	wrote := mustAppend(t, first, key, user("run-1", "question"), assistant("run-1", "answer"))

	second := thread.NewPostgres(poolOf(t, first))
	got, err := second.History(context.Background(), key, 2)
	if err != nil {
		t.Fatalf("History from a new store: %v", err)
	}
	if !reflect.DeepEqual(got, wrote) {
		t.Fatalf("the conversation changed across stores:\n got %+v\nwant %+v", got, wrote)
	}
	// A retry from the new process is still recognized, and a new run
	// continues from where the old process stopped.
	mustAppend(t, second, key, user("run-1", "question"), assistant("run-1", "answer"))
	next := mustAppend(t, second, key, user("run-2", "next"))
	if next[0].Seq != 3 {
		t.Fatalf("a new store continued at seq %d, want 3", next[0].Seq)
	}
}

// The journal is append-only by construction, and the constraints are the
// backstop for a writer that bypasses the store: a second entry for one run
// and role, or for one surface message, cannot be inserted behind its back.
func TestPostgresConstraintsRefuseADuplicateWrittenBehindTheStore(t *testing.T) {
	url := postgresURL(t)
	store := newPostgres(t, url)
	key := slackThread("docs-bot")
	q := user("run-1", "question")
	q.SurfaceID = "1699.002"
	mustAppend(t, store, key, q)

	insert := func(seq int, run, surfaceID string) error {
		_, err := poolOf(t, store).Exec(context.Background(),
			`INSERT INTO fiber_thread_entries
			   (tenant, agent, surface, seq, role, body, actor_id, actor_display, actor_bot, run_id, surface_id, at)
			 VALUES ($1, $2, $3, $4, 'user', 'forged', '', '', false, $5, $6, now())`,
			key.Tenant, key.Agent, key.Surface, seq, run, surfaceID)
		return err
	}
	if err := insert(1, "run-x", ""); err == nil {
		t.Error("a second entry at an existing sequence was accepted")
	}
	if err := insert(2, "run-1", ""); err == nil {
		t.Error("a second user entry for one run was accepted")
	}
	if err := insert(2, "", "1699.002"); err == nil {
		t.Error("a second entry for one surface message was accepted")
	}
}

// legacySchema is the journal as it shipped before entries carried
// attachments. The migration test builds it by hand, writes a row the old way,
// and then lets the current Migrate bring it forward.
const legacySchema = `
CREATE TABLE fiber_threads (
	tenant  text   NOT NULL,
	agent   text   NOT NULL,
	surface text   NOT NULL,
	head    bigint NOT NULL CHECK (head >= 0),
	PRIMARY KEY (tenant, agent, surface)
);
CREATE TABLE fiber_thread_entries (
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
`

// A journal written before entries had attachments is upgraded in place: the
// migration adds the column without touching a row, the old rows read back
// exactly as they were with no attachments, a pinned history over them is
// unchanged, and new entries with files append after them. Running the
// migration again changes nothing.
func TestPostgresMigrationAddsAttachmentsWithoutChangingOldEntries(t *testing.T) {
	url := postgresURL(t)
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_thread_legacy_%d_%d", time.Now().UnixNano(), schemas.Add(1))
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
		pool.Close()
	})
	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	key := slackThread("docs-bot")
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO fiber_threads VALUES ($1, $2, $3, 2)`, key.Tenant, key.Agent, key.Surface); err != nil {
		t.Fatal(err)
	}
	for i, row := range []struct{ role, body, run string }{{"user", "old question", "run-1"}, {"assistant", "old answer", "run-1"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO fiber_thread_entries VALUES ($1, $2, $3, $4, $5, $6, 'U1', 'ada', false, $7, '', $8)`,
			key.Tenant, key.Agent, key.Surface, i+1, row.role, row.body, row.run, at); err != nil {
			t.Fatal(err)
		}
	}

	store := thread.NewPostgres(pool)
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("Migrate over the legacy schema: %v", err)
		}
	}
	old, err := store.History(ctx, key, 2)
	if err != nil {
		t.Fatalf("History of legacy rows: %v", err)
	}
	if len(old) != 2 || old[0].Text != "old question" || old[1].Text != "old answer" || !old[0].At.Equal(at) {
		t.Fatalf("legacy rows read back as %+v", old)
	}
	for _, e := range old {
		if e.Attachments != nil {
			t.Errorf("legacy entry %d read back with attachments %#v, want nil", e.Seq, e.Attachments)
		}
	}

	q := user("run-2", "and this screenshot?")
	q.Attachments = []attachment.Attachment{{Digest: strings.Repeat("c", 64), MediaType: "image/png", Name: "s.png", Size: 9}}
	mustAppend(t, store, key, q)
	after, err := store.History(ctx, key, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, old) {
		t.Fatalf("history pinned over legacy rows changed after an append with files:\n got %+v\nwant %+v", after, old)
	}
	all := mustHistory(t, store, key)
	if len(all) != 3 || !reflect.DeepEqual(all[2].Attachments, q.Attachments) {
		t.Fatalf("new entry after migration = %+v", all)
	}
}

func postgresURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s to run the Postgres thread tests", postgresEnv)
	}
	return url
}
