package duraturo

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/duraturo/adapters/postgres/pgledger"
	"github.com/urmzd/duraturo/adapters/postgres/pgqueue"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// postgresEngine returns an engine on a fresh database of the server named by
// SAIGE_TEST_POSTGRES_DSN, with the adapters' suggested tables applied. Tests
// skip when the variable is unset.
func postgresEngine(t *testing.T) *Engine {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping Postgres-backed durable test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "saige_duraturo_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ddl := pgledger.RecommendedDDL(pgledger.DefaultMapping()) + "\n" + pgqueue.RecommendedDDL(pgqueue.DefaultMapping())
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	lgr, err := pgledger.New(pool, pgledger.DefaultMapping())
	if err != nil {
		t.Fatal(err)
	}
	q, err := pgqueue.New(pool, pgqueue.DefaultMapping(), pgqueue.WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lgr.Validate(ctx), q.Validate(ctx)); err != nil {
		t.Fatalf("validate schema: %v", err)
	}
	return New(lgr, q)
}

func TestPostgresSuspendDecideResume(t *testing.T) {
	e := postgresEngine(t)
	ctx := testContext(t)
	var calls, writes, reads atomic.Int32
	factory := func(string) *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) { writes.Add(1); return "written", nil }}
		read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: func(context.Context, map[string]any) (string, error) { reads.Add(1); return "read", nil }}
		return agent.NewAgent(agent.AgentConfig{Provider: provider{&calls}, SystemPrompt: "rules", Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), read)})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	input := []types.Message{types.UserMsg(types.Text("go"))}
	if _, err := e.Run(ctx, wf, "pg-run", input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if err := e.Decide(ctx, "pg-run", "marker/approval-call", "key", types.ApprovalDecision{Approved: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide(ctx, "pg-run", "marker/approval-call", "other", types.ApprovalDecision{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reply: %v", err)
	}
	result, err := e.Wait(ctx, "pg-run")
	if err != nil || result == nil {
		t.Fatalf("%v %v", result, err)
	}
	if writes.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("calls=%d writes=%d", calls.Load(), writes.Load())
	}
	if _, err := e.Run(ctx, wf, "pg-run", input); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || calls.Load() != 2 {
		t.Fatal("replayed effects")
	}
}
