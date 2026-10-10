package pgsource_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/definition/pgsource"
	"github.com/urmzd/saige/postgres"
)

// database returns a pool on a fresh, migrated database.
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "saige_agents_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func file(name, version, extra string) []byte {
	return []byte("---\napiVersion: saige/v1\nname: " + name + "\nversion: " + version + "\n" + extra + "---\nI am " + name + ".\n")
}

func TestPutLoadDelete(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	src := pgsource.New(pool, nil)
	if _, err := src.Put(ctx, file("helper", "1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Put(ctx, file("lead", "1.0.0", "subagents: [helper@^1]\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Put(ctx, []byte("---\nname: broken\n---\n")); !errors.Is(err, definition.ErrInvalid) {
		t.Fatalf("an invalid definition was stored: %v", err)
	}
	r := definition.NewRegistry(src, definition.Checks{})
	if _, err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("lead")
	if err != nil || res.Subagents[0].Agent.Name != "helper" || res.Source != src.String() {
		t.Fatalf("got %+v %v", res, err)
	}

	// Replacing a version keeps one row.
	if _, err := src.Put(ctx, file("helper", "1.0.0", "description: edited\n")); err != nil {
		t.Fatal(err)
	}
	defs, err := src.Load(ctx)
	if err != nil || len(defs) != 2 {
		t.Fatalf("got %d definitions, %v", len(defs), err)
	}

	removed, err := src.Delete(ctx, "helper", "1.0.0")
	if err != nil || !removed {
		t.Fatalf("delete: %v %v", removed, err)
	}
	if _, err := r.Load(ctx); err == nil || !strings.Contains(err.Error(), "helper") {
		t.Fatalf("a dangling reference loaded: %v", err)
	}
}

func TestLoadRejectsTamperedRows(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	src := pgsource.New(pool, nil)
	if _, err := src.Put(ctx, file("a", "1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE saige_agent_definitions SET body = body || 'more'`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Load(ctx); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE saige_agent_definitions SET version = '9.9.9'`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Load(ctx); err == nil || !strings.Contains(err.Error(), "holds") {
		t.Fatalf("got %v", err)
	}
}

// TestNotifyReload changes a row from another process's point of view, with
// plain SQL, and expects a watching registry to reload through LISTEN and
// NOTIFY, while a resolution pinned before the change stays as it was.
func TestNotifyReload(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	notifier := postgres.NewNotifier(pool, postgres.NotifierOptions{})
	defer func() { _ = notifier.Close(ctx) }()
	src := pgsource.New(pool, notifier)
	if _, err := src.Put(ctx, file("a", "1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	r := definition.NewRegistry(src, definition.Checks{})
	if _, err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	pinned, _ := r.Resolve("a")

	changed := make(chan struct{}, 4)
	stop, err := r.Watch(ctx, definition.WatchOptions{OnReload: func(c bool, err error) {
		if c && err == nil {
			changed <- struct{}{}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// The listener subscribes asynchronously; keep writing until a reload
	// arrives.
	other := pgsource.New(pool, nil)
	deadline := time.After(15 * time.Second)
	for i := 0; ; i++ {
		if _, err := other.Put(ctx, file("a", "1.1.0", fmt.Sprintf("description: write %d\n", i))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
		case <-time.After(300 * time.Millisecond):
			continue
		case <-deadline:
			t.Fatal("no reload after NOTIFY")
		}
		break
	}
	fresh, err := r.Resolve("a")
	if err != nil || fresh.Version != "1.1.0" {
		t.Fatalf("got %v %v", fresh, err)
	}
	if pinned.Version != "1.0.0" {
		t.Fatal("a reload changed a pinned resolution")
	}
	if got, ok := r.Pinned(pinned.Digest); !ok || got != pinned {
		t.Fatal("the pinned resolution is lost")
	}
}
