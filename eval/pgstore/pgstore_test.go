package pgstore_test

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

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/pgstore"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/storetest"
	"github.com/urmzd/saige/postgres"
)

// testPool connects to SAIGE_TEST_POSTGRES_DSN and runs migrations. Tests
// isolate themselves by tenant instead of truncating, so they can share the
// database with other packages' tests. They are skipped when the variable is
// unset:
//
//	SAIGE_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:5433/postgres go test ./eval/pgstore/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
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

func newStore(t *testing.T, pool *pgxpool.Pool) *pgstore.Store {
	t.Helper()
	s, err := pgstore.New(context.Background(), pgstore.Config{Pool: pool, Tenant: "test-" + rand.Text()[:10]})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	pool := testPool(t)
	storetest.Run(t, func(t *testing.T) store.Store { return newStore(t, pool) })
}

func TestTenantsAreIsolated(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	a, b := newStore(t, pool), newStore(t, pool)
	run := eval.RunRecord{ID: "shared", Suite: "s", Status: eval.RunRunning, StartedAt: time.Now()}
	if err := a.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetRun(ctx, "shared"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant GetRun = %v, want ErrNotFound", err)
	}
	if _, err := b.PutUnit(ctx, eval.Unit{RunID: "shared", Observation: eval.Observation{ID: "q"}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant PutUnit = %v, want ErrNotFound", err)
	}
	if err := b.CreateRun(ctx, run); err != nil {
		t.Fatalf("same run ID in another tenant: %v", err)
	}
	if _, err := a.PutUnit(ctx, eval.Unit{RunID: "shared", Observation: eval.Observation{ID: "q"},
		Scores: []eval.Score{{Name: "m", Value: 1}}}); err != nil {
		t.Fatal(err)
	}
	if units, err := b.Units(ctx, "shared", store.UnitFilter{}); err != nil || len(units) != 0 {
		t.Fatalf("other tenant Units = %d, %v; want none", len(units), err)
	}
	if runs, _ := b.ListRuns(ctx, store.RunFilter{}); len(runs) != 1 {
		t.Fatalf("other tenant ListRuns = %d runs, want its own 1", len(runs))
	}
	if h, _ := b.CaseHistory(ctx, "", "q", 0); len(h) != 0 {
		t.Fatalf("other tenant CaseHistory = %d entries, want none", len(h))
	}
}

func TestSchemaMissing(t *testing.T) {
	testPool(t) // migrates, so only the search path hides the tables
	ctx := context.Background()
	// A connection whose search_path hides public sees no eval tables.
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAIGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
	empty, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if _, err := pgstore.New(ctx, pgstore.Config{Pool: empty, Tenant: ""}); !errors.Is(err, pgstore.ErrSchemaMissing) {
		t.Fatalf("New without tables = %v, want ErrSchemaMissing", err)
	}
}

func saveRun(t *testing.T, s *pgstore.Store, id string, started time.Time, scores map[string]float64) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateRun(ctx, eval.RunRecord{ID: id, Suite: "s", Status: eval.RunSucceeded, StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	for obs, v := range scores {
		u := eval.Unit{RunID: id, Observation: eval.Observation{ID: obs}, Scores: []eval.Score{{Name: "m", Value: v}}}
		if _, err := s.PutUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCaseHistory(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	saveRun(t, s, "r1", base, map[string]float64{"q1": 0.2, "q2": 1})
	saveRun(t, s, "r2", base.Add(time.Hour), map[string]float64{"q1": 0.5})
	saveRun(t, s, "r3", base.Add(2*time.Hour), map[string]float64{"q2": 0})

	h, err := s.CaseHistory(ctx, "s", "q1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].RunID != "r2" || h[0].Unit.Scores[0].Value != 0.5 || h[1].RunID != "r1" {
		t.Fatalf("CaseHistory(q1) = %+v", h)
	}
	if h, _ := s.CaseHistory(ctx, "s", "q1", 1); len(h) != 1 || h[0].RunID != "r2" {
		t.Fatalf("CaseHistory limit 1 = %+v", h)
	}
	if h, _ := s.CaseHistory(ctx, "other", "q1", 0); len(h) != 0 {
		t.Fatalf("CaseHistory of another suite = %+v", h)
	}
}

func TestDiff(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	saveRun(t, s, "base", base, map[string]float64{"q1": 1, "q2": 0.5, "q3": 0.8})
	saveRun(t, s, "cand", base.Add(time.Hour), map[string]float64{"q1": 0.2, "q2": 0.9, "q4": 1})

	all, err := s.Diff(ctx, "base", "cand", pgstore.DiffFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("Diff = %d rows, want 4: %+v", len(all), all)
	}
	// Missing sides sort first, then the largest drop.
	if all[0].Candidate != nil && all[0].Base != nil {
		t.Fatalf("first row %+v, want a one-sided key", all[0])
	}
	if d, ok := all[2].Delta(); !ok || all[2].Key != "q1/0" || d > -0.79 {
		t.Fatalf("largest drop row = %+v", all[2])
	}

	reg, err := s.Diff(ctx, "base", "cand", pgstore.DiffFilter{Regressions: true, MinDrop: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, d := range reg {
		keys[d.Key] = true
	}
	if len(reg) != 2 || !keys["q1/0"] || !keys["q3/0"] {
		t.Fatalf("regressions = %+v, want q1 (dropped) and q3 (missing)", reg)
	}
	if _, err := s.Diff(ctx, "base", "nope", pgstore.DiffFilter{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Diff with a missing run = %v, want ErrNotFound", err)
	}
}

func TestReplaceRefreshesScoreIndex(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	saveRun(t, s, "a", base, map[string]float64{"q": 1})
	saveRun(t, s, "b", base.Add(time.Hour), map[string]float64{"q": 1})
	if _, err := s.PutUnit(ctx, eval.Unit{RunID: "b", Observation: eval.Observation{ID: "q"},
		Scores: []eval.Score{{Name: "m", Value: 0}}}); err != nil {
		t.Fatal(err)
	}
	reg, err := s.Diff(ctx, "a", "b", pgstore.DiffFilter{Regressions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(reg) != 1 || *reg[0].Candidate != 0 {
		t.Fatalf("diff after replacing the unit = %+v, want the new attempt's score", reg)
	}
}

func TestDiffLeavesOutInconclusiveCandidates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	saveRun(t, s, "base", base, map[string]float64{"q1": 1, "q2": 1, "q3": 1})
	if err := s.CreateRun(ctx, eval.RunRecord{ID: "cand", Suite: "s", Status: eval.RunSucceeded, StartedAt: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// q1: the judge's provider was down. q2: the subject never answered.
	// q3: the scorer failed for a real reason.
	unmeasured := eval.Observation{ID: "q2"}
	eval.MarkSubjectError(&unmeasured, eval.Infra(errors.New("connector unreachable")))
	for _, u := range []eval.Unit{
		{RunID: "cand", Observation: eval.Observation{ID: "q1"}, Scores: []eval.Score{{Name: "m", Error: "503", Inconclusive: true}}},
		{RunID: "cand", Observation: unmeasured},
		{RunID: "cand", Observation: eval.Observation{ID: "q3"}, Scores: []eval.Score{{Name: "m", Error: "bad output"}}},
	} {
		if _, err := s.PutUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.Diff(ctx, "base", "cand", pgstore.DiffFilter{})
	if err != nil {
		t.Fatal(err)
	}
	inconclusive := map[string]bool{}
	for _, d := range all {
		inconclusive[d.Key] = d.CandidateInconclusive
	}
	if !inconclusive["q1/0"] || !inconclusive["q2/0"] || inconclusive["q3/0"] {
		t.Fatalf("candidate inconclusive by key = %v, want q1 and q2 only: %+v", inconclusive, all)
	}

	reg, err := s.Diff(ctx, "base", "cand", pgstore.DiffFilter{Regressions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(reg) != 1 || reg[0].Key != "q3/0" || !reg[0].CandidateErrored {
		t.Fatalf("regressions = %+v, want only q3 (a real scorer failure)", reg)
	}

	units, err := s.Units(ctx, "cand", store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	suite := eval.SuiteFromUnits(eval.RunRecord{Suite: "s"}, units)
	if suite.Inconclusive != 2 {
		t.Fatalf("inconclusive units read back = %d, want 2", suite.Inconclusive)
	}
}

// freshDatabase creates an empty database on SAIGE_TEST_POSTGRES_DSN's server
// and returns a pool on it, so a test can change the schema without touching
// the tables other tests share.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testPool(t) // skips without a server
	ctx := context.Background()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "saige_eval_pgstore_" + strings.ToLower(rand.Text()[:12])
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
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: u.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestMigrationKeepsOlderScoresReadable migrates a database that an older
// release created, without eval_score.inconclusive, and checks that its rows
// read as conclusive and new rows can be written.
func TestMigrationKeepsOlderScoresReadable(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE eval_score DROP COLUMN inconclusive`); err != nil {
		t.Fatal(err)
	}
	s, err := pgstore.New(ctx, pgstore.Config{Pool: pool, Tenant: ""})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	// Write the old release's rows directly, as its PutUnit did.
	if err := s.CreateRun(ctx, eval.RunRecord{ID: "old", Suite: "s", Status: eval.RunSucceeded, StartedAt: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO eval_unit (tenant, run_id, key, attempt, observation_id, labels, recorded_at, unit)
		VALUES ('', 'old', 'q1/0', 1, 'q1', '{}', now(),
		        '{"run_id":"old","key":"q1/0","attempt":1,"observation":{"id":"q1","turn":0,"input":null,"output":null,"timing":{"total_ms":0}},"scores":[{"name":"m","value":0,"error":"timeout"}]}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO eval_score (tenant, run_id, key, name, value, errored, passed)
		VALUES ('', 'old', 'q1/0', 'm', 0, true, NULL)`); err != nil {
		t.Fatal(err)
	}

	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrating an older schema: %v", err)
	}
	saveRun(t, s, "new", base.Add(time.Hour), map[string]float64{"q1": 1})
	diffs, err := s.Diff(ctx, "new", "old", pgstore.DiffFilter{Regressions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || !diffs[0].CandidateErrored || diffs[0].CandidateInconclusive {
		t.Fatalf("diff against the older run = %+v, want its errored score as a conclusive regression", diffs)
	}
	loaded, err := store.LoadSuite(ctx, s, "old")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Inconclusive != 0 || len(loaded.Results) != 1 || loaded.Results[0].Scores[0].Error != "timeout" {
		t.Fatalf("older run read back as %+v", loaded)
	}
	if loaded.Gate(eval.Assertion{Metric: "m", Op: eval.GTE, Threshold: 1}) != eval.OutcomeFailed {
		t.Fatalf("older errored score gated as %s, want failed", loaded.Outcome)
	}
}
