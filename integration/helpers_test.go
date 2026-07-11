package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/postgres"
)

const (
	defaultChatModel  = "qwen3.5:4b"
	defaultEmbedModel = "nomic-embed-text"

	// migratedEmbedDim is the vector column width created by
	// postgres.RunMigrations with default MigrationOptions. Embedding tests
	// skip when the configured embed model produces a different width.
	migratedEmbedDim = 768
)

// requireOllama skips the test unless SAIGE_TEST_OLLAMA_HOST is set, then
// verifies the server is reachable and the configured models are pulled.
// A configured-but-broken environment fails loudly instead of skipping.
func requireOllama(t *testing.T) *ollama.Client {
	t.Helper()
	host := os.Getenv("SAIGE_TEST_OLLAMA_HOST")
	if host == "" {
		t.Skip("SAIGE_TEST_OLLAMA_HOST not set; skipping Ollama integration test")
	}
	model := envOr("SAIGE_TEST_OLLAMA_MODEL", defaultChatModel)
	embedModel := envOr("SAIGE_TEST_OLLAMA_EMBED_MODEL", defaultEmbedModel)

	tags := ollamaTags(t, host)
	for _, m := range []string{model, embedModel} {
		if !tags[m] {
			t.Fatalf("model %q not available on %s: run `ollama pull %s`", m, host, m)
		}
	}
	return ollama.NewClient(host, model, embedModel)
}

// ollamaTags fetches /api/tags and returns a lookup that matches both exact
// tag names and bare model names (e.g. "llama3.2" matches "llama3.2:latest").
func ollamaTags(t *testing.T, host string) map[string]bool {
	t.Helper()
	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(strings.TrimRight(host, "/") + "/api/tags")
	if err != nil {
		t.Fatalf("ollama unreachable at %s: %v", host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ollama %s/api/tags returned %s", host, resp.Status)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /api/tags: %v", err)
	}
	tags := make(map[string]bool, len(body.Models)*2)
	for _, m := range body.Models {
		tags[m.Name] = true
		if base, _, ok := strings.Cut(m.Name, ":"); ok {
			tags[base] = true
		}
	}
	return tags
}

// pgDSN skips the test unless SAIGE_TEST_POSTGRES_DSN is set. Point it at any
// pgvector-capable PostgreSQL: a local container or an AlloyDB instance.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}
	return dsn
}

// requirePostgres connects to SAIGE_TEST_POSTGRES_DSN, bootstraps the
// pgvector extension, and runs migrations. Mirrors agent/pgstore/store_test.go.
func requirePostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := pgDSN(t)
	ctx := context.Background()

	// postgres.NewPool registers pgvector types at connect time, so the
	// extension must exist before it can connect.
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		t.Fatalf("create vector extension: %v", err)
	}

	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return pool
}

func truncate(t *testing.T, pool *pgxpool.Pool, tables ...string) {
	t.Helper()
	sql := "TRUNCATE " + strings.Join(tables, ", ") + " CASCADE"
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// requireEmbedDim probes the embedding model and skips unless it matches the
// vector column width the migrations created.
func requireEmbedDim(t *testing.T, ctx context.Context, embedder *ollama.OllamaEmbedder) {
	t.Helper()
	vecs, err := embedder.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		t.Fatalf("probe embedding: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != migratedEmbedDim {
		t.Skipf("embed model produces %d-dim vectors but migrations created vector(%d) columns; "+
			"use a %d-dim model (e.g. %s) or re-migrate a fresh database with matching MigrationOptions",
			len(vecs[0]), migratedEmbedDim, migratedEmbedDim, defaultEmbedModel)
	}
}

// drainStream consumes an agent EventStream, returning the concatenated text,
// the names of every tool call the LLM made, and the stream's terminal error.
func drainStream(stream *agentsdk.EventStream) (string, []string, error) {
	var sb strings.Builder
	var toolCalls []string
	for d := range stream.Deltas() {
		switch v := d.(type) {
		case types.TextContentDelta:
			sb.WriteString(v.Content)
		case types.ToolCallStartDelta:
			toolCalls = append(toolCalls, v.Name)
		}
	}
	return sb.String(), toolCalls, stream.Wait()
}

// assistantText extracts the concatenated text content of an assistant message.
func assistantText(msg *types.AssistantMessage) string {
	if msg == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range msg.Content {
		if tc, ok := c.(types.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// addTool returns an arithmetic tool plus a pointer to its invocation counter.
func addTool() (*types.ToolFunc, *int) {
	calls := new(int)
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        "add",
			Description: "Add two numbers together and return the sum.",
			Parameters: types.ParameterSchema{
				Type:     "object",
				Required: []string{"a", "b"},
				Properties: map[string]types.PropertyDef{
					"a": {Type: "number", Description: "First number"},
					"b": {Type: "number", Description: "Second number"},
				},
			},
		},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			*calls++
			return fmt.Sprintf("%g", a+b), nil
		},
	}, calls
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func testContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
