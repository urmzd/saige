package online_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentpg "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
	"github.com/urmzd/saige/eval/pgstore"
	"github.com/urmzd/saige/postgres"
)

// freshPool creates an empty, migrated database on the server named by
// SAIGE_TEST_POSTGRES_DSN, so other packages truncating agent tables cannot
// interfere. The database is dropped on cleanup.
func freshPool(t *testing.T) *pgxpool.Pool {
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
	name := "saige_online_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	boot, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		t.Fatalf("create vector extension: %v", err)
	}
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: u.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return pool
}

// storedConversation records the support conversation through a scoped
// agent/pgstore store.
func storedConversation(t *testing.T, pool *pgxpool.Pool, scope, conv string) []types.NodeID {
	t.Helper()
	st, err := agentpg.New(agentpg.Config{Pool: pool, Scope: scope, ConversationID: conv})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := tree.New(types.SystemMsg(types.Text("You are a support agent.")), tree.WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	parent := tr.Root().ID
	var ids []types.NodeID
	for _, msg := range []types.Message{
		types.UserMsg(types.Text("What is the refund policy?")),
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.RoutePart{Model: "gpt-6-luna"},
			types.ToolCallPart{ID: "c1", Name: "lookup_policy", Arguments: map[string]any{}},
		}},
		types.SystemMessage{Parts: []types.SystemPart{types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("30 days")}}}},
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.RoutePart{Model: "gpt-6-luna"}, types.TextPart{Text: "Refunds within 30 days."},
		}},
	} {
		n, err := tr.AddChild(context.Background(), parent, msg)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
		parent = n.ID
	}
	return ids
}

func TestPGSourceIsScopedAndFindsFinishedRuns(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	ids := storedConversation(t, pool, "acme", "conv-1")
	storedConversation(t, pool, "other", "conv-1")

	src := online.PGSource{Pool: pool, Scope: "acme"}
	recs, err := src.Records(ctx, online.Window{From: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("Records = %d, want the one run of scope acme", len(recs))
	}
	r := recs[0]
	if r.Ref.Conversation != "conv-1" || r.Ref.Node != string(ids[3]) || r.Labels[online.LabelScope] != "acme" {
		t.Fatalf("record = %+v", r)
	}
	if r.Input != "What is the refund policy?" || r.Output != "Refunds within 30 days." || len(r.ToolCalls) != 1 || r.ToolCalls[0].Result != "30 days" {
		t.Fatalf("record content = %+v", r)
	}
	if all, _ := (online.PGSource{Pool: pool}).Records(ctx, online.Window{}); len(all) != 2 {
		t.Fatalf("unscoped Records = %d, want 2", len(all))
	}
	if later, _ := src.Records(ctx, online.Window{From: time.Now().Add(time.Hour)}); len(later) != 0 {
		t.Fatalf("future window = %d records", len(later))
	}
	if _, err := src.Lookup(ctx, online.Ref{Conversation: "conv-1", Node: string(ids[1])}); err == nil {
		t.Fatal("Lookup of a tool-calling node succeeded")
	}
	// A tool-calling turn stored by an earlier release, in the version 1
	// message format, is not a finished run either.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_node (uuid, parent_uuid, role, message, branch_id, conversation_id)
		VALUES ('v1-call', '', 'assistant', '{"content":[{"type":"tool_use","data":{"ID":"c","Name":"f"}}]}'::jsonb, 'main', 'legacy')`); err != nil {
		t.Fatal(err)
	}
	if all, _ := (online.PGSource{Pool: pool}).Records(ctx, online.Window{}); len(all) != 2 {
		t.Fatalf("Records with a version 1 tool call = %d, want 2", len(all))
	}
}

// TestWatchWithPostgresNotifier runs the long-running mode across real
// LISTEN/NOTIFY into the Postgres eval store.
func TestWatchWithPostgresNotifier(t *testing.T) {
	pool := freshPool(t)
	ids := storedConversation(t, pool, "acme", "conv-1")
	results, err := pgstore.New(context.Background(), pgstore.Config{Pool: pool, Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	n := postgres.NewNotifier(pool, postgres.NotifierOptions{})
	defer n.Close()

	s := &online.Sampler{Store: results, Scorers: []eval.Scorer{eval.ContainsScorer("30 days")}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	scored := make(chan eval.Unit, 1)
	done := make(chan error, 1)
	go func() {
		_, err := s.Watch(ctx, n, online.PGSource{Pool: pool, Scope: "acme"}, online.WatchOptions{
			OnReady: func(id string) { ready <- id },
			OnUnit:  func(u eval.Unit) { scored <- u },
		})
		done <- err
	}()
	runID := <-ready
	// LISTEN is issued asynchronously; announce until the watcher hears it.
	deadline := time.After(10 * time.Second)
	var unit eval.Unit
wait:
	for {
		if err := online.Announce(ctx, n, "", online.Ref{Conversation: "conv-1", Node: string(ids[3])}); err != nil {
			t.Fatal(err)
		}
		select {
		case unit = <-scored:
			break wait
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for the announced run to be scored")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if unit.Observation.Labels[online.LabelNode] != string(ids[3]) || unit.Scores[0].Value != 1 {
		t.Fatalf("unit = %+v", unit)
	}
	run, err := results.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != eval.RunSucceeded || run.Units != 1 || run.Labels[online.LabelSource] != online.SourceOnline {
		t.Fatalf("run = %+v", run)
	}
	h, err := results.CaseHistory(context.Background(), online.DefaultSuite, string(ids[3]), 0)
	if err != nil || len(h) != 1 {
		t.Fatalf("CaseHistory = %+v, %v", h, err)
	}
}
