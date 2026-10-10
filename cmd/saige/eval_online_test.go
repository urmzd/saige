package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentpg "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
	"github.com/urmzd/saige/eval/store"
)

func TestOnlineWindow(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	w, err := onlineWindow(evalOnlineFlags{since: time.Hour}, now)
	if err != nil || !w.From.Equal(now.Add(-time.Hour)) || !w.To.IsZero() {
		t.Fatalf("since window = %+v, %v", w, err)
	}
	w, err = onlineWindow(evalOnlineFlags{from: "2026-05-01T00:00:00Z", to: "2026-05-02T00:00:00Z"}, now)
	if err != nil || w.From.Day() != 1 || w.To.Day() != 2 {
		t.Fatalf("from/to window = %+v, %v", w, err)
	}
	if _, err := onlineWindow(evalOnlineFlags{from: "2026-05-02T00:00:00Z", to: "2026-05-01T00:00:00Z"}, now); err == nil {
		t.Fatal("an inverted window was accepted")
	}
	if _, err := onlineWindow(evalOnlineFlags{from: "yesterday"}, now); err == nil {
		t.Fatal("a malformed --from was accepted")
	}
}

func TestOnlineFilterAndScorers(t *testing.T) {
	f, err := onlineFilter(evalOnlineFlags{labels: []string{"env=prod"}, errored: "true", models: []string{"m"}})
	if err != nil || f.Where["env"] != "prod" || f.Errored == nil || !*f.Errored || f.Models[0] != "m" {
		t.Fatalf("filter = %+v, %v", f, err)
	}
	if _, err := onlineFilter(evalOnlineFlags{labels: []string{"env"}}); err == nil {
		t.Fatal("a label without a value was accepted")
	}
	if _, err := onlineFilter(evalOnlineFlags{errored: "maybe"}); err == nil {
		t.Fatal("--errored maybe was accepted")
	}
	scorers, err := buildOnlineScorers([]string{"tool_success_rate", `{"kind": "contains", "params": {"substrings": ["x"]}}`})
	if err != nil || len(scorers) != 2 {
		t.Fatalf("scorers = %v, %v", scorers, err)
	}
	if _, err := buildOnlineScorers([]string{"no_such_kind"}); err == nil {
		t.Fatal("an unknown scorer kind was accepted")
	}
}

func TestOpenEvalStoreRejectsTenantForDirectory(t *testing.T) {
	if _, _, err := openEvalStore(context.Background(), t.TempDir(), "acme", false); err == nil {
		t.Fatal("--tenant with a directory store was accepted")
	}
	if _, _, err := openEvalStore(context.Background(), filepath.Join(t.TempDir(), "missing"), "", false); err == nil {
		t.Fatal("a missing directory was opened without create")
	}
}

// freshEvalDSN creates an empty database on SAIGE_TEST_POSTGRES_DSN's server
// and returns its URL, so these tests never race other packages that
// truncate agent tables. It skips when the variable is unset.
func freshEvalDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "saige_cli_eval_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
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
		t.Fatal(err)
	}
	return u.String()
}

func TestEvalRunAndRunsWithPostgresStore(t *testing.T) {
	dsn := freshEvalDSN(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "doc"}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer server.Close()
	corpus := t.TempDir()
	for name, body := range map[string]string{"system.md": "sys", "turn-0.md": "synth"} {
		path := filepath.Join(corpus, "001", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := newEvalRunCmd(context.Background())
	cmd.SetArgs([]string{
		"--experiments-dir", corpus, "--api-base", server.URL, "--api-key", "k",
		"--model", "mock", "--flows", "base", "--force", "--store", dsn, "--tenant", "acme",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	s, closeStore, err := openEvalStore(context.Background(), dsn, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	runs, err := s.ListRuns(context.Background(), store.RunFilter{})
	if err != nil || len(runs) != 1 || runs[0].Status != eval.RunSucceeded || runs[0].Units != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}

	out := captureStdout(t, func() {
		ls := newEvalRunsCmd()
		ls.SetArgs([]string{"--store", dsn, "--tenant", "acme"})
		if err := ls.Execute(); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, runs[0].ID) {
		t.Fatalf("eval runs output lacks the run:\n%s", out)
	}
	other := captureStdout(t, func() {
		ls := newEvalRunsCmd()
		ls.SetArgs([]string{"--store", dsn, "--tenant", "other"})
		if err := ls.Execute(); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(other, runs[0].ID) {
		t.Fatalf("another tenant sees the run:\n%s", other)
	}
}

func TestEvalOnlineSweepsStoredConversations(t *testing.T) {
	dsn := freshEvalDSN(t)
	ctx := context.Background()
	pool, err := connectPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st, err := agentpg.New(agentpg.Config{Pool: pool, Scope: "acme", ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := tree.New(types.SystemMsg(types.Text("sys")), tree.WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	parent := tr.Root().ID
	for _, msg := range []types.Message{
		types.UserMsg(types.Text("Cancel order 42; my email is jane.doe@example.com")),
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "c1", Name: "cancel_order", Arguments: map[string]any{}}}},
		types.SystemMessage{Parts: []types.SystemPart{types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("unavailable")}, IsError: true}}},
		types.AssistantMessage{Parts: []types.AssistantPart{types.TextPart{Text: "Sorry, I could not cancel it."}}},
	} {
		n, err := tr.AddChild(ctx, parent, msg)
		if err != nil {
			t.Fatal(err)
		}
		parent = n.ID
	}

	promoted := filepath.Join(t.TempDir(), "cases.jsonl")
	var out bytes.Buffer
	cmd := newEvalOnlineCmd(ctx)
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--store", dsn, "--tenant", "acme", "--scope", "acme",
		"--scorer", "tool_success_rate", "--errored", "true",
		"--promote", promoted, "--promote-metric", "tool_success_rate",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`scored\s+1\n`).MatchString(out.String()) || !strings.Contains(out.String(), "tool_success_rate") {
		t.Fatalf("report:\n%s", out.String())
	}
	data, err := os.ReadFile(promoted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "jane.doe@example.com") || !strings.Contains(string(data), "[REDACTED:EMAIL]") {
		t.Fatalf("promoted cases:\n%s", data)
	}

	s, closeStore, err := openEvalStore(ctx, dsn, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	runs, err := s.ListRuns(ctx, store.RunFilter{Suite: online.DefaultSuite})
	if err != nil || len(runs) != 1 || runs[0].Labels[online.LabelSource] != online.SourceOnline || runs[0].Units != 1 {
		t.Fatalf("online runs = %+v, %v", runs, err)
	}
}
