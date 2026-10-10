package pgstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/memory"
	"github.com/urmzd/saige/agent/memory/memorytest"
	agentpg "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/postgres"
)

const embDim = 768

var (
	dbOnce sync.Once
	dbPool *pgxpool.Pool
	dbErr  error
	dbDrop func()
)

// TestMain drops the package's database after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if dbDrop != nil {
		dbDrop()
	}
	os.Exit(code)
}

// testPool returns a pool on a database of its own, created once per run on
// the server named by SAIGE_TEST_POSTGRES_DSN, migrated, with memory_record
// emptied for each test. A database of its own keeps other packages' tests,
// which truncate agent tables, from racing these. Tests are skipped when the
// variable is unset. A disposable server:
//
//	docker run --rm -e POSTGRES_PASSWORD=test -p 5433:5432 paradedb/paradedb:0.26.1-pg18
//	SAIGE_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:5433/postgres go test ./agent/memory/pgstore/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}
	dbOnce.Do(func() { dbPool, dbDrop, dbErr = freshDatabase(dsn) })
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if _, err := dbPool.Exec(context.Background(), `TRUNCATE memory_record`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return dbPool
}

func freshDatabase(dsn string) (*pgxpool.Pool, func(), error) {
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	name := "saige_memory_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("create database: %w", err)
	}
	drop := func() {
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close()
	}
	u, err := url.Parse(dsn)
	if err != nil {
		drop()
		return nil, nil, err
	}
	u.Path = "/" + name
	// postgres.NewPool registers pgvector types at connect time, so the
	// extension must exist first.
	boot, err := pgxpool.New(ctx, u.String())
	if err != nil {
		drop()
		return nil, nil, err
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		drop()
		return nil, nil, err
	}
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: u.String()})
	if err != nil {
		drop()
		return nil, nil, err
	}
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{MemoryEmbeddingDim: embDim}); err != nil {
		pool.Close()
		drop()
		return nil, nil, fmt.Errorf("migrations: %w", err)
	}
	return pool, func() { pool.Close(); drop() }, nil
}

// conceptEmbedder is a deterministic stand-in for an embedding model. Each
// word maps to a concept, synonyms share one, and the concept picks a
// dimension, so texts that share a meaning but no words are close, and
// texts with nothing in common are orthogonal.
type conceptEmbedder struct {
	mu    sync.Mutex
	calls int
	texts int
}

var synonyms = map[string]string{
	"car": "vehicle", "automobile": "vehicle", "drive": "vehicle", "sedan": "vehicle",
	"deploy": "release", "deploys": "release", "deploying": "release", "deployment": "release", "ship": "release", "release": "release",
	"tea": "beverage", "coffee": "beverage", "drink": "beverage",
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "my": true, "i": true, "do": true, "what": true,
	"in": true, "of": true, "on": true, "to": true, "with": true, "we": true, "did": true, "about": true,
	"user": true, "assistant": true,
}

func (e *conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.texts += len(texts)
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, embDim)
		for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) {
			if stopwords[w] {
				continue
			}
			if c, ok := synonyms[w]; ok {
				w = c
			}
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[h.Sum32()%(embDim-1)]++
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm == 0 {
			v[embDim-1] = 1 // only stopwords: a vector unrelated to every word
			norm = 1
		}
		for j := range v {
			v[j] = float32(float64(v[j]) / math.Sqrt(norm))
		}
		out[i] = v
	}
	return out, nil
}

func newStore(t *testing.T, cfg Config) (*Store, *conceptEmbedder) {
	t.Helper()
	pool := testPool(t)
	emb := &conceptEmbedder{}
	cfg.Embedder = emb
	if cfg.MinSimilarity == 0 {
		cfg.MinSimilarity = 0.3
	}
	s, err := New(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, emb
}

var (
	tenant = memorytest.Tenant
	other  = memorytest.Other
)

func TestConformance(t *testing.T) {
	memorytest.RunConformance(t, func(t *testing.T) memory.Store {
		s, _ := newStore(t, Config{})
		return s
	})
}

func TestNewRequiresEmbedder(t *testing.T) {
	if _, err := New(&pgxpool.Pool{}, Config{}); err == nil {
		t.Fatal("New without an embedder succeeded")
	}
}

func contents(recs []memory.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Content
	}
	return out
}

// TestHybridRecall checks that each search finds what the other cannot: the
// vector search a record that shares a meaning but no words with the query,
// and BM25 an exact identifier the embedder does not tell apart.
func TestHybridRecall(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, Config{})
	for _, c := range []string{
		"my automobile is a blue sedan",
		"prefers green tea in the afternoon",
		"the on-call rotation changes on Mondays",
		"incident ticket ZX4471 is about the login outage",
	} {
		if _, err := s.Remember(ctx, memory.Record{Scope: tenant, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, query, want string
	}{
		{"semantic match without shared words", "what car do I drive", "my automobile is a blue sedan"},
		{"semantic match ranks first", "favorite drink coffee", "prefers green tea in the afternoon"},
		{"lexical match of an identifier", "ZX4471", "incident ticket ZX4471 is about the login outage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, err := s.Recall(ctx, tenant, tc.query, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) == 0 || recs[0].Content != tc.want {
				t.Fatalf("Recall(%q) = %q, want %q first", tc.query, contents(recs), tc.want)
			}
		})
	}
	if recs, _ := s.Recall(ctx, tenant, "piano lessons", 0); len(recs) != 0 {
		t.Fatalf("unrelated query recalled %q", contents(recs))
	}
}

func TestRecencyBreaksTies(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s, _ := newStore(t, Config{})
	old := memory.Record{Scope: tenant, Content: "standup is at nine", CreatedAt: now.Add(-48 * time.Hour)}
	recent := memory.Record{Scope: tenant, Content: "standup is at ten", CreatedAt: now.Add(-time.Hour)}
	for _, r := range []memory.Record{old, recent} {
		if _, err := s.Remember(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := s.Recall(ctx, tenant, "standup", 0)
	if err != nil || len(recs) != 2 || recs[0].Content != recent.Content {
		t.Fatalf("Recall = %q, %v; want the newer record first", contents(recs), err)
	}
}

func TestSearchFilters(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s, _ := newStore(t, Config{})
	for _, r := range []memory.Record{
		{Scope: tenant, Kind: memory.KindProcedural, Content: "release by tagging main", CreatedAt: now.Add(-72 * time.Hour)},
		{Scope: tenant, Kind: memory.KindEpisodic, Content: "the last release failed on a flaky test", CreatedAt: now.Add(-time.Hour)},
	} {
		if _, err := s.Remember(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		q    Query
		want []string
	}{
		{"kind", Query{Text: "release", Kinds: []memory.Kind{memory.KindProcedural}}, []string{"release by tagging main"}},
		{"since", Query{Text: "release", Since: now.Add(-24 * time.Hour)}, []string{"the last release failed on a flaky test"}},
		{"until", Query{Text: "release", Until: now.Add(-24 * time.Hour)}, []string{"release by tagging main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, err := s.Search(ctx, tenant, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := contents(recs); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("Search = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScopeIsolation writes the same note under several scopes and checks
// that no search, list, or delete crosses a tenant or subject, and that a
// narrowed scope sees only its own namespace and those below it.
func TestScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, Config{})
	scopes := map[string]memory.Scope{
		"acme user":        tenant,
		"acme other user":  {Tenant: "acme", Subject: "user-2"},
		"globex same user": other,
		"acme agent":       tenant.Narrow("agent-a"),
		"acme sub-agent":   tenant.Narrow("agent-a").Narrow("worker"),
		"acme sibling":     tenant.Narrow("agent-b"),
	}
	ids := map[string]string{}
	for name, sc := range scopes {
		id, err := s.Remember(ctx, memory.Record{Scope: sc, Content: "the vault code lives in the safe " + name, IdempotencyKey: "same-key"})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	for _, tc := range []struct {
		scope string
		want  int
	}{
		{"acme user", 4}, // itself and its three namespaces
		{"acme other user", 1},
		{"globex same user", 1},
		{"acme agent", 2},
		{"acme sub-agent", 1},
		{"acme sibling", 1},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			sc := scopes[tc.scope]
			for _, q := range []string{"vault code safe", ""} {
				recs, err := s.Recall(ctx, sc, q, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(recs) != tc.want {
					t.Fatalf("Recall(%q) = %q, want %d records", q, contents(recs), tc.want)
				}
				for _, r := range recs {
					if r.Scope.Tenant != sc.Tenant || r.Scope.Subject != sc.Subject {
						t.Fatalf("recalled a record of %+v from %+v", r.Scope, sc)
					}
				}
			}
		})
	}
	if err := s.Forget(ctx, other, ids["acme user"]); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("cross-tenant forget err = %v", err)
	}
	if err := s.Forget(ctx, scopes["acme sibling"], ids["acme agent"]); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("sibling forget err = %v", err)
	}
	if _, err := s.Recall(ctx, memory.Scope{Tenant: "acme", Namespace: "a/../b"}, "x", 0); !errors.Is(err, memory.ErrNoScope) {
		t.Fatalf("invalid namespace err = %v", err)
	}
}

// TestIdempotentReplay replays tool writes through the memory tools, as a
// resumed durable run does, and checks that the embedder ran once and one
// row was stored.
func TestIdempotentReplay(t *testing.T) {
	s, emb := newStore(t, Config{})
	policy := memory.Policy{AutoApprove: true, Scope: func(context.Context, string) (memory.Scope, error) { return tenant, nil }}
	var remember types.Tool
	for _, tl := range memory.Tools(s, policy) {
		if tl.Definition().Name == memory.RememberToolName {
			remember = tl
		}
	}
	ctx := types.WithToolCallInfo(context.Background(), types.ToolCallInfo{ID: "call-42", Name: memory.RememberToolName, Agent: "lead"})
	args := map[string]any{"content": "the staging database is read-only on Fridays"}
	first, err := remember.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := remember.Execute(ctx, args)
		if err != nil || again != first {
			t.Fatalf("replay = %q, %v; want %q", again, err, first)
		}
	}
	if emb.texts != 1 {
		t.Fatalf("embedder ran for %d texts, want 1", emb.texts)
	}
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM memory_record`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want 1", n, err)
	}
}

func TestRetentionAndPurge(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s, _ := newStore(t, Config{Now: func() time.Time { return now }})
	p := memory.Policy{Retention: time.Hour, Now: func() time.Time { return now.Add(-2 * time.Hour) }}
	if _, err := p.Remember(ctx, s, memory.Record{Scope: tenant, Content: "temporary badge number expires soon"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, memory.Record{Scope: other, Content: "kept badge note"}); err != nil {
		t.Fatal(err)
	}
	if recs, _ := s.Recall(ctx, tenant, "badge", 0); len(recs) != 0 {
		t.Fatalf("expired record recalled: %q", contents(recs))
	}
	n, err := s.Purge(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Purge = %d, %v; want 1", n, err)
	}
	if recs, _ := s.Recall(ctx, other, "badge", 0); len(recs) != 1 {
		t.Fatalf("Purge removed an unexpired record: %q", contents(recs))
	}
}

func TestSelectorMode(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, Config{})
	for _, c := range []string{"prefers tabs over spaces", "uses vim keybindings", "the build uses make"} {
		if _, err := s.Remember(ctx, memory.Record{Scope: tenant, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	p := memory.Policy{Recall: memory.RecallBySelector, Scope: func(context.Context, string) (memory.Scope, error) { return tenant, nil }}
	msg, ok, err := p.StartMessage(ctx, s, "lead", "which keybindings")
	if err != nil || !ok {
		t.Fatalf("StartMessage = %v, %v", ok, err)
	}
	text := msg.Content[0].(types.TextContent).Text
	if !memory.IsInjected(text) || !strings.Contains(text, "vim keybindings") || strings.Contains(text, "make") {
		t.Fatalf("injected = %q", text)
	}
}

func TestSearchRejectsDimensionMismatch(t *testing.T) {
	pool := testPool(t)
	s, err := New(pool, Config{Embedder: shortEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(context.Background(), memory.Record{Scope: tenant, Content: "x"}); err == nil {
		t.Fatal("a vector of the wrong dimension was stored")
	}
}

// shortEmbedder returns three-dimensional vectors, narrower than the column.
type shortEmbedder struct{}

func (shortEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// saveTurns writes a conversation to agent/pgstore as a chain of nodes.
func saveTurns(t *testing.T, pool *pgxpool.Pool, conversationID string, msgs ...types.Message) {
	t.Helper()
	store := agentpg.NewStore(pool, conversationID, nil)
	now := time.Now().UTC().Truncate(time.Microsecond)
	parent := types.NodeID("")
	for i, m := range msgs {
		id := types.NodeID(fmt.Sprintf("%s-n%d", strings.ReplaceAll(conversationID, "\x1f", "_"), i))
		n := &types.Node{ID: id, ParentID: parent, Message: m, State: types.NodeActive, Version: 1, Depth: i,
			BranchID: "main", CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now}
		if err := store.SaveNode(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		parent = id
	}
}

func TestConversationRecall(t *testing.T) {
	ctx := context.Background()
	s, emb := newStore(t, Config{ConversationRetention: 24 * time.Hour})
	pool := s.pool
	conv, err := agentpg.ScopedConversationID("acme", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	saveTurns(t, pool, conv,
		memory.InjectRecords([]memory.Record{{ID: "m", Kind: memory.KindSemantic, Content: "an injected note"}}),
		types.NewUserMessage("How should we deploy the billing service? Mail me at ada@example.com."),
		types.AssistantMessage{Content: []types.AssistantContent{
			types.TextContent{Text: "Ship it behind a feature flag, then ramp to 10 percent. I noted <<PHONE_1>>."},
			types.ToolUseContent{ID: "t1", Name: "noop", Arguments: map[string]any{}},
		}},
		types.NewUserMessage("Also, my favorite drink is coffee."),
	)

	n, err := s.IndexConversation(ctx, tenant, conv)
	if err != nil || n != 3 {
		t.Fatalf("IndexConversation = %d, %v; want 3 turns", n, err)
	}
	calls := emb.calls
	if n, err := s.IndexConversation(ctx, tenant, conv); err != nil || n != 0 || emb.calls != calls {
		t.Fatalf("re-index = %d, %v, embed calls %d -> %d; want nothing new", n, err, calls, emb.calls)
	}

	recs, err := s.Search(ctx, tenant, Query{Text: "deployment of billing", Conversations: true})
	if err != nil || len(recs) == 0 {
		t.Fatalf("conversation search = %q, %v", contents(recs), err)
	}
	all, _ := s.Search(ctx, tenant, Query{Conversations: true})
	joined := strings.Join(contents(all), "\n")
	for _, bad := range []string{"ada@example.com", "<<PHONE_1>>", "an injected note"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("indexed text holds %q:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "[REDACTED:EMAIL]") || !strings.Contains(joined, "[REDACTED:PHONE]") {
		t.Fatalf("indexed text not redacted:\n%s", joined)
	}
	if !strings.HasPrefix(recs[0].Content, "user: How should we deploy") && !strings.HasPrefix(recs[0].Content, "assistant: Ship it") {
		t.Fatalf("top turn = %q", recs[0].Content)
	}
	if recs[0].Source.Conversation != conv || recs[0].Kind != memory.KindEpisodic || recs[0].ExpiresAt.IsZero() {
		t.Fatalf("turn record = %+v", recs[0])
	}

	// Memories and turns are recalled separately.
	if mem, _ := s.Recall(ctx, tenant, "deploy billing", 0); len(mem) != 0 {
		t.Fatalf("memory recall returned turns: %q", contents(mem))
	}
	if got, _ := s.Search(ctx, other, Query{Text: "deploy billing", Conversations: true}); len(got) != 0 {
		t.Fatalf("another tenant recalled turns: %q", contents(got))
	}
	if _, err := s.IndexConversation(ctx, other, conv); !errors.Is(err, ErrForeignConversation) {
		t.Fatalf("foreign index err = %v", err)
	}

	// The tool resolves the scope from the policy and wraps results.
	policy := memory.Policy{Recall: memory.RecallByInjection, Scope: func(_ context.Context, owner string) (memory.Scope, error) {
		if owner == "outsider" {
			return other, nil
		}
		return tenant, nil
	}}
	tool := ConversationTool(s, policy)
	call := func(agent string) string {
		ctx := types.WithToolCallInfo(ctx, types.ToolCallInfo{ID: "c", Name: ConversationToolName, Agent: agent})
		out, err := tool.Execute(ctx, map[string]any{"query": "what did we discuss about deploying billing", "budget": float64(200)})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := call("lead"); !strings.HasPrefix(out, "<memory-") || !strings.Contains(out, "deploy the billing service") {
		t.Fatalf("tool = %q", out)
	}
	if out := call("outsider"); out != "no matching conversation turns" {
		t.Fatalf("outsider tool = %q", out)
	}

	// Inject at start, within the budget.
	policy.InjectBudget = 40
	msg, ok, err := policy.StartMessage(ctx, s.Conversations(), "lead", "deploy billing service")
	if err != nil || !ok {
		t.Fatalf("StartMessage = %v, %v", ok, err)
	}
	text := msg.Content[0].(types.TextContent).Text
	if !memory.IsInjected(text) || !strings.Contains(text, "billing") || strings.Contains(text, "coffee") {
		t.Fatalf("injected = %q", text)
	}
	if _, err := s.Conversations().Remember(ctx, memory.Record{Scope: tenant, Content: "x"}); !errors.Is(err, memory.ErrUnsupported) {
		t.Fatalf("view remember err = %v", err)
	}

	if n, err := s.ForgetConversation(ctx, tenant, conv); err != nil || n != 3 {
		t.Fatalf("ForgetConversation = %d, %v", n, err)
	}
	if got, _ := s.Search(ctx, tenant, Query{Conversations: true}); len(got) != 0 {
		t.Fatalf("turns left after ForgetConversation: %q", contents(got))
	}
}
