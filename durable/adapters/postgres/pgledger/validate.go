package pgledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/durable/adapters/postgres/internal/pgident"
	"github.com/urmzd/saige/durable/adapters/postgres/internal/pginspect"
)

// envelopeTypes are the column types an envelope may have. jsonb is
// recommended; text (and varchar) columns holding JSON are tolerated.
var envelopeTypes = map[string]bool{"jsonb": true, "text": true, "varchar": true}

// createdTypes are the column types a mapped CreatedAt column may have.
var createdTypes = map[string]bool{"timestamptz": true, "timestamp": true}

// Validate introspects the live database and checks the mapping against it:
// both tables exist, every mapped column exists, the envelope columns are
// jsonb (or text), Runs.RunID is covered by a plain UNIQUE constraint or
// unique index, and (Records.RunID, Records.Key) is covered by one. Plain
// means non-partial, non-expression, non-deferrable: Accept's and Record's
// ON CONFLICT arbiter inference accepts nothing else, so a laxer check here
// would green-light a schema that then fails at runtime. It never modifies
// anything; every error says exactly what to CREATE or ALTER.
func (l *Ledger) Validate(ctx context.Context) error {
	var errs []error

	runs, err := pginspect.Inspect(ctx, l.pool, l.m.Runs.Table)
	if err != nil {
		return fmt.Errorf("pgledger: validate: %w", err)
	}
	if !runs.Exists {
		errs = append(errs, fmt.Errorf("runs table %q does not exist; create it, for example:\n%s",
			l.m.Runs.Table, runsDDL(l.m.Runs)))
	} else {
		errs = append(errs, l.validateRuns(runs)...)
	}

	recs, err := pginspect.Inspect(ctx, l.pool, l.m.Records.Table)
	if err != nil {
		return fmt.Errorf("pgledger: validate: %w", err)
	}
	if !recs.Exists {
		errs = append(errs, fmt.Errorf("records table %q does not exist; create it, for example:\n%s",
			l.m.Records.Table, recordsDDL(l.m.Records)))
	} else {
		errs = append(errs, l.validateRecords(recs)...)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pgledger: validate: %w", err)
	}
	return nil
}

func (l *Ledger) validateRuns(t pginspect.TableInfo) []error {
	var errs []error
	tab := l.m.Runs.Table

	requireColumn(&errs, t, tab, "Mapping.Runs.RunID", l.m.Runs.RunID, "text")
	requireColumn(&errs, t, tab, "Mapping.Runs.Status", l.m.Runs.Status, "text")
	if requireColumn(&errs, t, tab, "Mapping.Runs.Envelope", l.m.Runs.Envelope, "jsonb") {
		typ := t.Columns[l.m.Runs.Envelope]
		if !envelopeTypes[typ] {
			errs = append(errs, fmt.Errorf(
				"column %q of %q (Mapping.Runs.Envelope) is %s, want jsonb (or text); fix it: ALTER TABLE %s ALTER COLUMN %s TYPE jsonb USING %s::jsonb",
				l.m.Runs.Envelope, tab, typ,
				pgident.QuoteTable(tab), l.qRunsEnv, l.qRunsEnv))
		} else if typ == "jsonb" {
			l.envMode.Store(envModeJSONB)
		} else {
			l.envMode.Store(envModeText)
		}
	}
	if l.m.Runs.CreatedAt != "" {
		if requireColumn(&errs, t, tab, "Mapping.Runs.CreatedAt", l.m.Runs.CreatedAt, "timestamptz") {
			if typ := t.Columns[l.m.Runs.CreatedAt]; !createdTypes[typ] {
				errs = append(errs, fmt.Errorf(
					"column %q of %q (Mapping.Runs.CreatedAt) is %s, want timestamptz",
					l.m.Runs.CreatedAt, tab, typ))
			}
		}
	}
	if l.m.Runs.Scope.Column != "" {
		requireColumn(&errs, t, tab, "Mapping.Runs.Scope.Column", l.m.Runs.Scope.Column, "text")
	}

	if !t.HasUniqueOn(l.m.Runs.RunID) {
		errs = append(errs, fmt.Errorf(
			"runs table %q has no plain (non-partial, non-expression, non-deferrable) UNIQUE constraint or index on (%s) — the only kind ON CONFLICT can use; create one: CREATE UNIQUE INDEX ON %s (%s)",
			tab, l.m.Runs.RunID, pgident.QuoteTable(tab), l.qRunsID))
	}
	return errs
}

func (l *Ledger) validateRecords(t pginspect.TableInfo) []error {
	var errs []error
	tab := l.m.Records.Table

	requireColumn(&errs, t, tab, "Mapping.Records.RunID", l.m.Records.RunID, "text")
	requireColumn(&errs, t, tab, "Mapping.Records.Key", l.m.Records.Key, "text")
	if requireColumn(&errs, t, tab, "Mapping.Records.Envelope", l.m.Records.Envelope, "jsonb") {
		if typ := t.Columns[l.m.Records.Envelope]; !envelopeTypes[typ] {
			errs = append(errs, fmt.Errorf(
				"column %q of %q (Mapping.Records.Envelope) is %s, want jsonb (or text); fix it: ALTER TABLE %s ALTER COLUMN %s TYPE jsonb USING %s::jsonb",
				l.m.Records.Envelope, tab, typ,
				pgident.QuoteTable(tab), l.qRecsEnv, l.qRecsEnv))
		}
	}
	if l.m.Records.Scope.Column != "" {
		requireColumn(&errs, t, tab, "Mapping.Records.Scope.Column", l.m.Records.Scope.Column, "text")
	}

	if !t.HasUniqueOn(l.m.Records.RunID, l.m.Records.Key) {
		errs = append(errs, fmt.Errorf(
			"records table %q has no plain (non-partial, non-expression, non-deferrable) UNIQUE constraint or index on (%s, %s) — the only kind ON CONFLICT can use; create one: CREATE UNIQUE INDEX ON %s (%s, %s)",
			tab, l.m.Records.RunID, l.m.Records.Key,
			pgident.QuoteTable(tab), l.qRecsID, l.qRecsKey))
	}
	return errs
}

// requireColumn appends an actionable error when col is absent and reports
// whether it exists. suggestedType only feeds the ALTER TABLE suggestion.
func requireColumn(errs *[]error, t pginspect.TableInfo, table, field, col, suggestedType string) bool {
	if _, ok := t.Columns[col]; ok {
		return true
	}
	*errs = append(*errs, fmt.Errorf(
		"table %q has no column %q (%s); add it: ALTER TABLE %s ADD COLUMN %s %s",
		table, col, field, pgident.QuoteTable(table), pgident.QuoteColumn(col), suggestedType))
	return false
}

// RecommendedDDL returns suggested CREATE statements matching m. It is
// advice returned as a string, never executed by this package: review it and
// apply it with your own migration tooling, or hand it to pgkit.Apply.
// Existing tables that already satisfy Validate need none of it.
//
// Run IDs and record keys are COLLATE "C": they are opaque byte strings, the
// janitor pages pending runs in (created_at, run_id) order, and prefix reads
// scan record keys, so they compare bytewise exactly as Go compares strings
// (and as the in-memory ledger orders them) instead of by a locale's rules.
// Validate accepts any collation; only ordering, not correctness, depends on
// it.
func RecommendedDDL(m Mapping) string {
	return runsDDL(m.Runs) + "\n" + recordsDDL(m.Records)
}

func runsDDL(r RunsMap) string {
	var b strings.Builder
	b.WriteString("-- Runs table for pgledger. Suggested only: pgledger never executes DDL.\n")
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", pgident.QuoteTable(r.Table))
	fmt.Fprintf(&b, "    %s text COLLATE \"C\" NOT NULL,\n", pgident.QuoteColumn(r.RunID))
	fmt.Fprintf(&b, "    %s text NOT NULL,\n", pgident.QuoteColumn(r.Status))
	fmt.Fprintf(&b, "    %s jsonb NOT NULL", pgident.QuoteColumn(r.Envelope))
	if r.CreatedAt != "" {
		fmt.Fprintf(&b, ",\n    %s timestamptz NOT NULL DEFAULT now()", pgident.QuoteColumn(r.CreatedAt))
	}
	if r.Scope.Column != "" {
		fmt.Fprintf(&b, ",\n    %s text NOT NULL", pgident.QuoteColumn(r.Scope.Column))
	}
	b.WriteString("\n);\n")
	fmt.Fprintf(&b, "CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s);\n",
		pgident.QuoteColumn(indexName(r.Table, r.RunID, "key")),
		pgident.QuoteTable(r.Table), pgident.QuoteColumn(r.RunID))
	if r.CreatedAt != "" {
		// The janitor's whole scan: status equality, then the
		// (created_at, run_id) order and cursor, served by the index.
		fmt.Fprintf(&b, "CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s, %s);\n",
			pgident.QuoteColumn(indexName(r.Table, "pending", "idx")),
			pgident.QuoteTable(r.Table), pgident.QuoteColumn(r.Status), pgident.QuoteColumn(r.CreatedAt), pgident.QuoteColumn(r.RunID))
	} else {
		fmt.Fprintf(&b, "CREATE INDEX IF NOT EXISTS %s ON %s (%s);\n",
			pgident.QuoteColumn(indexName(r.Table, "pending", "idx")),
			pgident.QuoteTable(r.Table), pgident.QuoteColumn(r.Status))
	}
	return b.String()
}

func recordsDDL(r RecordsMap) string {
	var b strings.Builder
	b.WriteString("-- Records table for pgledger. Suggested only: pgledger never executes DDL.\n")
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", pgident.QuoteTable(r.Table))
	fmt.Fprintf(&b, "    %s text COLLATE \"C\" NOT NULL,\n", pgident.QuoteColumn(r.RunID))
	fmt.Fprintf(&b, "    %s text COLLATE \"C\" NOT NULL,\n", pgident.QuoteColumn(r.Key))
	fmt.Fprintf(&b, "    %s jsonb NOT NULL", pgident.QuoteColumn(r.Envelope))
	if r.Scope.Column != "" {
		fmt.Fprintf(&b, ",\n    %s text NOT NULL", pgident.QuoteColumn(r.Scope.Column))
	}
	b.WriteString("\n);\n")
	fmt.Fprintf(&b, "CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s, %s);\n",
		pgident.QuoteColumn(indexName(r.Table, r.RunID+"_"+r.Key, "key")),
		pgident.QuoteTable(r.Table), pgident.QuoteColumn(r.RunID), pgident.QuoteColumn(r.Key))
	return b.String()
}

// indexName derives an index identifier from a table name (schema stripped)
// and a suffix, truncated to PostgreSQL's 63-byte identifier limit.
func indexName(table, middle, suffix string) string {
	base := table
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[i+1:]
	}
	name := base + "_" + middle + "_" + suffix
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
