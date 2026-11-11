package pgqueue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lazyPool returns a pool that parses but never connects: pgxpool dials
// lazily, so constructor-only tests need no database.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://unit:unit@127.0.0.1:1/unit")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestNewRejectsNilPool(t *testing.T) {
	if _, err := New(nil, DefaultMapping()); err == nil {
		t.Fatal("New(nil pool) = nil error, want error")
	}
}

func TestNewValidatesMapping(t *testing.T) {
	pool := lazyPool(t)

	tests := []struct {
		name    string
		mutate  func(m *Mapping)
		wantSub string
	}{
		{"missing table", func(m *Mapping) { m.Table = "" }, "Mapping.Table"},
		{"missing run id", func(m *Mapping) { m.RunID = "" }, "Mapping.RunID"},
		{"missing ready at", func(m *Mapping) { m.ReadyAt = "" }, "Mapping.ReadyAt"},
		{"missing lease until", func(m *Mapping) { m.LeaseUntil = "" }, "Mapping.LeaseUntil"},
		{"missing attempt", func(m *Mapping) { m.Attempt = "" }, "Mapping.Attempt"},
		{"missing failures", func(m *Mapping) { m.Failures = "" }, "Mapping.Failures"},
		{"unsafe table", func(m *Mapping) { m.Table = `q"; DROP TABLE q; --` }, "unsafe identifier"},
		{"unsafe column", func(m *Mapping) { m.ReadyAt = "ready at" }, "unsafe identifier"},
		{"mixed-case table", func(m *Mapping) { m.Table = "MyQueue" }, "lower_snake_case"},
		{"mixed-case schema part", func(m *Mapping) { m.Table = "App.queue" }, "lower_snake_case"},
		{"mixed-case column", func(m *Mapping) { m.ReadyAt = "ReadyAt" }, "lower_snake_case"},
		{"scope column without value", func(m *Mapping) { m.Scope = Scope{Column: "source"} }, "Scope.Value is required"},
		{"scope value without column", func(m *Mapping) { m.Scope = Scope{Value: "anchor"} }, "Scope.Column is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := DefaultMapping()
			tt.mutate(&m)
			_, err := New(pool, m)
			if err == nil {
				t.Fatal("New = nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("New error = %q, want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

func TestWithPollInterval(t *testing.T) {
	pool := lazyPool(t)

	q, err := New(pool, DefaultMapping(), WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New with WithPollInterval(5ms) = %v, want nil", err)
	}
	if q.poll != 5*time.Millisecond {
		t.Fatalf("poll interval = %v, want 5ms", q.poll)
	}

	if _, err := New(pool, DefaultMapping(), WithPollInterval(0)); err == nil {
		t.Fatal("New with WithPollInterval(0) = nil error, want error")
	}
	if _, err := New(pool, DefaultMapping(), WithPollInterval(-time.Second)); err == nil {
		t.Fatal("New with negative poll interval = nil error, want error")
	}

	q, err = New(pool, DefaultMapping())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if q.poll != defaultPollInterval {
		t.Fatalf("default poll interval = %v, want %v", q.poll, defaultPollInterval)
	}
}

func TestDefaultMapping(t *testing.T) {
	m := DefaultMapping()
	if m.Table != "durable_queue" {
		t.Fatalf("DefaultMapping table = %q, want durable_queue", m.Table)
	}
	if err := m.validate(); err != nil {
		t.Fatalf("DefaultMapping does not validate: %v", err)
	}
	if m.Scope.Column != "" {
		t.Fatal("DefaultMapping should not set a scope")
	}
}

func TestRecommendedDDLGolden(t *testing.T) {
	const want = `-- Queue table for pgqueue. Suggested only: pgqueue never executes DDL.
CREATE TABLE IF NOT EXISTS "durable_queue" (
    "run_id" text COLLATE "C" NOT NULL,
    "ready_at" timestamptz,
    "lease_until" timestamptz,
    "attempt" bigint NOT NULL DEFAULT 0,
    "failures" int NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS "durable_queue_run_id_key" ON "durable_queue" ("run_id");
CREATE INDEX IF NOT EXISTS "durable_queue_claimable_idx" ON "durable_queue" ("ready_at", "lease_until");
`
	got := RecommendedDDL(DefaultMapping())
	if got != want {
		t.Fatalf("RecommendedDDL(DefaultMapping()) mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRecommendedDDLScoped(t *testing.T) {
	m := DefaultMapping()
	m.Scope = Scope{Column: "source", Value: "anchor"}
	got := RecommendedDDL(m)
	if !strings.Contains(got, `"source" text NOT NULL`) {
		t.Errorf("RecommendedDDL missing scope column in:\n%s", got)
	}
}

func TestAnswerContext(t *testing.T) {
	type key struct{}
	t.Run("LiveWhileCallerLives", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
		defer cancel()
		stmtCtx, stop := answerContext(ctx, time.Hour)
		if stmtCtx.Value(key{}) != "v" {
			t.Fatal("answer context lost the caller's values")
		}
		if _, ok := stmtCtx.Deadline(); ok {
			t.Fatal("answer context carries a deadline while the caller lives")
		}
		stop()
		if stmtCtx.Err() == nil {
			t.Fatal("stop did not cancel the answer context")
		}
	})

	t.Run("OutlivesCancelByWait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		const wait = 300 * time.Millisecond
		stmtCtx, stop := answerContext(ctx, wait)
		defer stop()
		cancel()
		time.Sleep(wait / 10)
		if stmtCtx.Err() != nil {
			t.Fatal("answer context died with its caller; it must give the answer time to arrive")
		}
		select {
		case <-stmtCtx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("answer context outlived its caller by far more than wait")
		}
	})

	t.Run("CallerDeadlineStartsTheWait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		stmtCtx, stop := answerContext(ctx, time.Hour)
		defer stop()
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		if stmtCtx.Err() != nil {
			t.Fatal("answer context expired with the caller's deadline")
		}
	})
}
