package pgqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/durable/adapters/postgres/internal/pgident"
	"github.com/urmzd/saige/durable/adapters/postgres/internal/pginspect"
)

// timeTypes are the column types ReadyAt and LeaseUntil may have.
var timeTypes = map[string]bool{"timestamptz": true, "timestamp": true}

// intTypes are the column types Attempt and Failures may have.
var intTypes = map[string]bool{"int2": true, "int4": true, "int8": true}

// Validate introspects the live database and checks the mapping against it:
// the table exists, every mapped column exists with a workable type, and
// RunID is covered by a plain UNIQUE constraint or unique index —
// non-partial, non-expression, non-deferrable. Only plain unique indexes
// count because Enqueue's ON CONFLICT arbiter inference accepts nothing
// else: a partial or expression index would pass a laxer check here and
// then fail at runtime. It never modifies anything; every error says
// exactly what to CREATE or ALTER.
func (q *Queue) Validate(ctx context.Context) error {
	t, err := pginspect.Inspect(ctx, q.pool, q.m.Table)
	if err != nil {
		return fmt.Errorf("pgqueue: validate: %w", err)
	}
	if !t.Exists {
		return fmt.Errorf("pgqueue: validate: queue table %q does not exist; create it, for example:\n%s",
			q.m.Table, RecommendedDDL(q.m))
	}

	var errs []error
	requireCol := func(field, col, wantTypes string, ok map[string]bool) {
		typ, exists := t.Columns[col]
		if !exists {
			errs = append(errs, fmt.Errorf(
				"table %q has no column %q (%s); add it: ALTER TABLE %s ADD COLUMN %s %s",
				q.m.Table, col, field, pgident.QuoteTable(q.m.Table), pgident.QuoteColumn(col), wantTypes))
			return
		}
		if ok != nil && !ok[typ] {
			errs = append(errs, fmt.Errorf(
				"column %q of %q (%s) is %s, want %s",
				col, q.m.Table, field, typ, wantTypes))
		}
	}

	requireCol("Mapping.RunID", q.m.RunID, "text", nil)
	requireCol("Mapping.ReadyAt", q.m.ReadyAt, "timestamptz", timeTypes)
	requireCol("Mapping.LeaseUntil", q.m.LeaseUntil, "timestamptz", timeTypes)
	requireCol("Mapping.Attempt", q.m.Attempt, "bigint", intTypes)
	requireCol("Mapping.Failures", q.m.Failures, "int", intTypes)
	if q.m.Scope.Column != "" {
		requireCol("Mapping.Scope.Column", q.m.Scope.Column, "text", nil)
	}

	if !t.HasUniqueOn(q.m.RunID) {
		errs = append(errs, fmt.Errorf(
			"queue table %q has no plain (non-partial, non-expression, non-deferrable) UNIQUE constraint or index on (%s) — the only kind ON CONFLICT can use; create one: CREATE UNIQUE INDEX ON %s (%s)",
			q.m.Table, q.m.RunID, pgident.QuoteTable(q.m.Table), q.qID))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pgqueue: validate: %w", err)
	}
	return nil
}

// RecommendedDDL returns suggested CREATE statements matching m. It is
// advice returned as a string, never executed by this package: review it and
// apply it with your own migration tooling, or hand it to pgkit.Apply. The
// run ID is COLLATE "C", byte-ordered like every durable key (see
// pgledger.RecommendedDDL).
func RecommendedDDL(m Mapping) string {
	var b strings.Builder
	b.WriteString("-- Queue table for pgqueue. Suggested only: pgqueue never executes DDL.\n")
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", pgident.QuoteTable(m.Table))
	fmt.Fprintf(&b, "    %s text COLLATE \"C\" NOT NULL,\n", pgident.QuoteColumn(m.RunID))
	fmt.Fprintf(&b, "    %s timestamptz,\n", pgident.QuoteColumn(m.ReadyAt))
	fmt.Fprintf(&b, "    %s timestamptz,\n", pgident.QuoteColumn(m.LeaseUntil))
	fmt.Fprintf(&b, "    %s bigint NOT NULL DEFAULT 0,\n", pgident.QuoteColumn(m.Attempt))
	fmt.Fprintf(&b, "    %s int NOT NULL DEFAULT 0", pgident.QuoteColumn(m.Failures))
	if m.Scope.Column != "" {
		fmt.Fprintf(&b, ",\n    %s text NOT NULL", pgident.QuoteColumn(m.Scope.Column))
	}
	b.WriteString("\n);\n")
	fmt.Fprintf(&b, "CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s);\n",
		pgident.QuoteColumn(indexName(m.Table, m.RunID, "key")),
		pgident.QuoteTable(m.Table), pgident.QuoteColumn(m.RunID))
	fmt.Fprintf(&b, "CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s);\n",
		pgident.QuoteColumn(indexName(m.Table, "claimable", "idx")),
		pgident.QuoteTable(m.Table), pgident.QuoteColumn(m.ReadyAt), pgident.QuoteColumn(m.LeaseUntil))
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

// bytesTypes are the column types the delta Payload may have.
var bytesTypes = map[string]bool{"bytea": true}

// Validate checks the queue mapping (as Queue.Validate does) and the delta
// mapping against the live database: the delta table exists and every
// mapped column exists with a workable type. It never modifies anything.
func (d *DeltaQueue) Validate(ctx context.Context) error {
	var errs []error
	if err := d.Queue.Validate(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := d.validateDeltaTable(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (d *DeltaQueue) validateDeltaTable(ctx context.Context) error {
	t, err := pginspect.Inspect(ctx, d.pool, d.dm.Table)
	if err != nil {
		return fmt.Errorf("pgqueue: validate delta table: %w", err)
	}
	if !t.Exists {
		return fmt.Errorf("pgqueue: validate: delta table %q does not exist; create it, for example:\n%s",
			d.dm.Table, RecommendedDeltaDDL(d.dm))
	}

	var errs []error
	requireCol := func(field, col, wantTypes string, ok map[string]bool) {
		typ, exists := t.Columns[col]
		if !exists {
			errs = append(errs, fmt.Errorf(
				"table %q has no column %q (%s); add it: ALTER TABLE %s ADD COLUMN %s %s",
				d.dm.Table, col, field, pgident.QuoteTable(d.dm.Table), pgident.QuoteColumn(col), wantTypes))
			return
		}
		if ok != nil && !ok[typ] {
			errs = append(errs, fmt.Errorf(
				"column %q of %q (%s) is %s, want %s",
				col, d.dm.Table, field, typ, wantTypes))
		}
	}
	requireCol("DeltaMapping.Seq", d.dm.Seq, "bigint GENERATED ALWAYS AS IDENTITY", intTypes)
	requireCol("DeltaMapping.RunID", d.dm.RunID, "text", nil)
	requireCol("DeltaMapping.RecordKey", d.dm.RecordKey, "text", nil)
	requireCol("DeltaMapping.Attempt", d.dm.Attempt, "bigint", intTypes)
	requireCol("DeltaMapping.Kind", d.dm.Kind, "text", nil)
	requireCol("DeltaMapping.Payload", d.dm.Payload, "bytea", bytesTypes)

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pgqueue: validate: %w", err)
	}
	return nil
}

// RecommendedDeltaDDL returns suggested CREATE statements for the delta
// table described by dm. Like RecommendedDDL it is advice returned as a
// string, never executed by this package.
func RecommendedDeltaDDL(dm DeltaMapping) string {
	var b strings.Builder
	b.WriteString("-- Delta log table for pgqueue. Suggested only: pgqueue never executes DDL.\n")
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", pgident.QuoteTable(dm.Table))
	fmt.Fprintf(&b, "    %s bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,\n", pgident.QuoteColumn(dm.Seq))
	fmt.Fprintf(&b, "    %s text COLLATE \"C\" NOT NULL,\n", pgident.QuoteColumn(dm.RunID))
	fmt.Fprintf(&b, "    %s text NOT NULL,\n", pgident.QuoteColumn(dm.RecordKey))
	fmt.Fprintf(&b, "    %s bigint NOT NULL,\n", pgident.QuoteColumn(dm.Attempt))
	fmt.Fprintf(&b, "    %s text NOT NULL,\n", pgident.QuoteColumn(dm.Kind))
	fmt.Fprintf(&b, "    %s bytea NOT NULL\n", pgident.QuoteColumn(dm.Payload))
	b.WriteString(");\n")
	fmt.Fprintf(&b, "CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s);\n",
		pgident.QuoteColumn(indexName(dm.Table, dm.RunID+"_"+dm.Seq, "idx")),
		pgident.QuoteTable(dm.Table), pgident.QuoteColumn(dm.RunID), pgident.QuoteColumn(dm.Seq))
	return b.String()
}
