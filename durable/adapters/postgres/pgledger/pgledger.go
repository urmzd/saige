// Package pgledger implements durable's ledger.Ledger on PostgreSQL tables
// the application already owns.
//
// durable requires no additional database and no tables of its own. The
// adapter is configured with a declarative Mapping naming the caller's
// tables and columns: New checks the mapping's shape, Validate introspects
// the live database and reports exactly what is missing (down to the CREATE
// statement that would fix it), and RecommendedDDL returns suggested CREATE
// statements as a string for the caller to apply through their own migration
// tooling. Nothing in this package ever executes DDL — the adapter
// validates, it never creates or migrates.
//
// Storage model: the full run.Run / run.Record struct is stored as JSON in
// the mapped Envelope column — a jsonb column, or a text column holding
// JSON. Dedicated mapped columns (run ID, status, optionally created-at) are
// written alongside the envelope and take precedence when reading, so the
// caller's existing indexes, constraints, and reports on those columns keep
// working. []byte fields appear base64-encoded inside the envelope, per
// encoding/json.
//
// Shared tables: set Scope to discriminate durable's rows inside a table
// that also holds other data. Set Defaults to fill the caller's own NOT NULL
// columns on insert.
package pgledger

import (
	"errors"

	"github.com/urmzd/saige/durable/adapters/postgres/internal/pgident"
	"github.com/urmzd/saige/durable/run"
)

// Scope is an optional discriminator for tables shared with non-anchor
// rows. When Column is set, every row the adapter writes carries Value in
// that column and every statement filters on it, so the adapter sees only
// its own slice of the table.
type Scope struct {
	Column string
	Value  string
}

// RunsMap declares which existing table and columns hold runs.
type RunsMap struct {
	// Table is the runs table, optionally schema-qualified. Required.
	Table string
	// RunID is the column carrying the run ID. It must be covered by a
	// plain (non-partial, non-expression, non-deferrable) UNIQUE
	// constraint or unique index (Validate checks). Required.
	RunID string
	// Status is a text column holding 'pending', 'succeeded', or
	// 'failed'. Required.
	Status string
	// Envelope is a jsonb (or text) column holding the full run.Run as
	// JSON; every field without a dedicated mapped column lives only
	// here. Required.
	Envelope string
	// CreatedAt optionally maps a dedicated timestamptz column. When
	// empty, creation time is read from the envelope.
	CreatedAt string
	// Scope optionally discriminates rows in a shared table.
	Scope Scope
	// Defaults, when set, is called on insert to fill the caller's own
	// NOT NULL columns: keys are column names (validated like every
	// other identifier and refused if they collide with mapped columns),
	// values are passed as query parameters.
	Defaults func(r run.Run) map[string]any
}

// RecordsMap declares which existing table and columns hold records.
type RecordsMap struct {
	// Table is the records table, optionally schema-qualified. Required.
	Table string
	// RunID is the column carrying the owning run's ID. Required.
	RunID string
	// Key is the record-key column; (RunID, Key) must be covered by a
	// plain (non-partial, non-expression, non-deferrable) UNIQUE
	// constraint or unique index (Validate checks). Required.
	Key string
	// Envelope is a jsonb (or text) column holding the full run.Record
	// as JSON. Required.
	Envelope string
	// Scope optionally discriminates rows in a shared table.
	Scope Scope
	// Defaults fills the caller's own NOT NULL columns on insert.
	Defaults func(rec run.Record) map[string]any
}

// Mapping wires a Ledger onto existing tables.
type Mapping struct {
	Runs    RunsMap
	Records RecordsMap
}

// DefaultMapping returns the mapping matching RecommendedDDL's suggested
// durable_runs / durable_records tables.
func DefaultMapping() Mapping {
	return Mapping{
		Runs: RunsMap{
			Table:     "durable_runs",
			RunID:     "run_id",
			Status:    "status",
			Envelope:  "envelope",
			CreatedAt: "created_at",
		},
		Records: RecordsMap{
			Table:    "durable_records",
			RunID:    "run_id",
			Key:      "record_key",
			Envelope: "envelope",
		},
	}
}

// validate checks that all required identifiers are present and safe. It is
// the only gate between mapping strings and SQL text.
func (m Mapping) validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	add(pgident.Table("Mapping.Runs.Table", m.Runs.Table))
	add(pgident.Column("Mapping.Runs.RunID", m.Runs.RunID))
	add(pgident.Column("Mapping.Runs.Status", m.Runs.Status))
	add(pgident.Column("Mapping.Runs.Envelope", m.Runs.Envelope))
	if m.Runs.CreatedAt != "" {
		add(pgident.Column("Mapping.Runs.CreatedAt", m.Runs.CreatedAt))
	}
	add(validateScope("Mapping.Runs.Scope", m.Runs.Scope))

	add(pgident.Table("Mapping.Records.Table", m.Records.Table))
	add(pgident.Column("Mapping.Records.RunID", m.Records.RunID))
	add(pgident.Column("Mapping.Records.Key", m.Records.Key))
	add(pgident.Column("Mapping.Records.Envelope", m.Records.Envelope))
	add(validateScope("Mapping.Records.Scope", m.Records.Scope))

	return errors.Join(errs...)
}

func validateScope(field string, s Scope) error {
	if s.Column == "" && s.Value == "" {
		return nil
	}
	if s.Column == "" {
		return errors.New(field + ".Column is required when Scope.Value is set")
	}
	if s.Value == "" {
		return errors.New(field + ".Value is required when Scope.Column is set")
	}
	return pgident.Column(field+".Column", s.Column)
}
