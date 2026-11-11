package pgledger

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable/ledger"
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

// Ledger offers the optional prefix-read capability.
var _ ledger.PrefixReader = (*Ledger)(nil)

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
		{"missing runs table", func(m *Mapping) { m.Runs.Table = "" }, "Mapping.Runs.Table"},
		{"missing runs run id", func(m *Mapping) { m.Runs.RunID = "" }, "Mapping.Runs.RunID"},
		{"missing runs status", func(m *Mapping) { m.Runs.Status = "" }, "Mapping.Runs.Status"},
		{"missing runs envelope", func(m *Mapping) { m.Runs.Envelope = "" }, "Mapping.Runs.Envelope"},
		{"missing records table", func(m *Mapping) { m.Records.Table = "" }, "Mapping.Records.Table"},
		{"missing records run id", func(m *Mapping) { m.Records.RunID = "" }, "Mapping.Records.RunID"},
		{"missing records key", func(m *Mapping) { m.Records.Key = "" }, "Mapping.Records.Key"},
		{"missing records envelope", func(m *Mapping) { m.Records.Envelope = "" }, "Mapping.Records.Envelope"},
		{"unsafe table", func(m *Mapping) { m.Runs.Table = `runs"; DROP TABLE users; --` }, "unsafe identifier"},
		{"unsafe column", func(m *Mapping) { m.Runs.Status = "status, envelope" }, "unsafe identifier"},
		{"unsafe created at", func(m *Mapping) { m.Runs.CreatedAt = "created at" }, "unsafe identifier"},
		{"mixed-case table", func(m *Mapping) { m.Runs.Table = "AppRuns" }, "lower_snake_case"},
		{"mixed-case schema part", func(m *Mapping) { m.Runs.Table = "App.runs" }, "lower_snake_case"},
		{"mixed-case column", func(m *Mapping) { m.Records.Key = "RecordKey" }, "lower_snake_case"},
		{"deeply qualified table", func(m *Mapping) { m.Runs.Table = "a.b.c" }, "more than one dot"},
		{"scope column without value", func(m *Mapping) { m.Runs.Scope = Scope{Column: "source"} }, "Scope.Value is required"},
		{"scope value without column", func(m *Mapping) { m.Records.Scope = Scope{Value: "anchor"} }, "Scope.Column is required"},
		{"unsafe scope column", func(m *Mapping) { m.Runs.Scope = Scope{Column: "so urce", Value: "x"} }, "unsafe identifier"},
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

func TestNewAcceptsValidMappings(t *testing.T) {
	pool := lazyPool(t)

	m := DefaultMapping()
	if _, err := New(pool, m); err != nil {
		t.Fatalf("New(DefaultMapping) = %v, want nil", err)
	}

	m = DefaultMapping()
	m.Runs.Table = "app.durable_runs" // schema-qualified
	m.Runs.CreatedAt = ""             // optional column unmapped
	m.Runs.Scope = Scope{Column: "source", Value: "anchor"}
	m.Records.Scope = Scope{Column: "source", Value: "anchor"}
	if _, err := New(pool, m); err != nil {
		t.Fatalf("New(scoped, qualified, no CreatedAt) = %v, want nil", err)
	}
}

func TestDefaultMapping(t *testing.T) {
	m := DefaultMapping()
	if m.Runs.Table != "durable_runs" || m.Records.Table != "durable_records" {
		t.Fatalf("DefaultMapping tables = %q / %q, want durable_runs / durable_records",
			m.Runs.Table, m.Records.Table)
	}
	if err := m.validate(); err != nil {
		t.Fatalf("DefaultMapping does not validate: %v", err)
	}
	if m.Runs.CreatedAt == "" {
		t.Fatal("DefaultMapping should map a dedicated CreatedAt column")
	}
	if m.Runs.Scope.Column != "" || m.Records.Scope.Column != "" {
		t.Fatal("DefaultMapping should not set a scope")
	}
}

func TestRecommendedDDLGolden(t *testing.T) {
	const want = `-- Runs table for pgledger. Suggested only: pgledger never executes DDL.
CREATE TABLE IF NOT EXISTS "durable_runs" (
    "run_id" text COLLATE "C" NOT NULL,
    "status" text NOT NULL,
    "envelope" jsonb NOT NULL,
    "created_at" timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS "durable_runs_run_id_key" ON "durable_runs" ("run_id");
CREATE INDEX IF NOT EXISTS "durable_runs_pending_idx" ON "durable_runs" ("status", "created_at", "run_id");

-- Records table for pgledger. Suggested only: pgledger never executes DDL.
CREATE TABLE IF NOT EXISTS "durable_records" (
    "run_id" text COLLATE "C" NOT NULL,
    "record_key" text COLLATE "C" NOT NULL,
    "envelope" jsonb NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS "durable_records_run_id_record_key_key" ON "durable_records" ("run_id", "record_key");
`
	got := RecommendedDDL(DefaultMapping())
	if got != want {
		t.Fatalf("RecommendedDDL(DefaultMapping()) mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRecommendedDDLScoped(t *testing.T) {
	m := DefaultMapping()
	m.Runs.CreatedAt = ""
	m.Runs.Scope = Scope{Column: "source", Value: "anchor"}
	m.Records.Scope = Scope{Column: "source", Value: "anchor"}
	got := RecommendedDDL(m)

	for _, sub := range []string{
		`"source" text NOT NULL`,
		`CREATE INDEX IF NOT EXISTS "durable_runs_pending_idx" ON "durable_runs" ("status");`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("RecommendedDDL missing %q in:\n%s", sub, got)
		}
	}
	if strings.Contains(got, "created_at") {
		t.Errorf("RecommendedDDL mentions created_at with CreatedAt unmapped:\n%s", got)
	}
}
