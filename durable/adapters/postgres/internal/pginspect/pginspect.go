// Package pginspect reads table shape out of the PostgreSQL catalogs so the
// adapters can validate a Mapping against the database the caller already
// owns. It only ever reads: nothing in this module executes DDL.
package pginspect

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableInfo is the shape of one table as the adapters care about it.
type TableInfo struct {
	// Exists reports whether the table resolves at all (respecting
	// search_path for unqualified names).
	Exists bool
	// Columns maps column name to pg_type.typname (e.g. "text", "jsonb",
	// "timestamptz", "int8").
	Columns map[string]string
	// Uniques holds the column-name set of every PLAIN unique index: one
	// that ON CONFLICT (cols) arbiter inference accepts. Partial (indpred),
	// expression-keyed, deferrable (NOT indimmediate), and invalid indexes
	// are excluded — ON CONFLICT cannot use them, so counting one here
	// would green-light a schema that then fails at runtime.
	Uniques [][]string
}

// Inspect loads TableInfo for table (optionally schema-qualified). A missing
// table yields Exists == false, not an error.
func Inspect(ctx context.Context, pool *pgxpool.Pool, table string) (TableInfo, error) {
	var oid *int64
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::oid::bigint`, table).Scan(&oid); err != nil {
		return TableInfo{}, fmt.Errorf("pginspect: resolve table %q: %w", table, err)
	}
	if oid == nil {
		return TableInfo{}, nil
	}

	info := TableInfo{Exists: true, Columns: make(map[string]string)}

	rows, err := pool.Query(ctx, `
		SELECT a.attname, t.typname
		FROM pg_attribute a
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`, *oid)
	if err != nil {
		return TableInfo{}, fmt.Errorf("pginspect: columns of %q: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return TableInfo{}, fmt.Errorf("pginspect: columns of %q: %w", table, err)
		}
		info.Columns[name] = typ
	}
	if err := rows.Err(); err != nil {
		return TableInfo{}, fmt.Errorf("pginspect: columns of %q: %w", table, err)
	}

	// One row per unique index that ON CONFLICT arbiter inference accepts:
	// unique, non-partial (indpred IS NULL), immediate (non-deferrable),
	// and valid. Expression key columns (indkey attnum 0) join to NULL and
	// are removed, which makes the length check below reject the index.
	idxRows, err := pool.Query(ctx, `
		SELECT i.indnkeyatts::int,
		       coalesce(array_remove(array_agg(a.attname::text ORDER BY k.ord), NULL), '{}')
		FROM pg_index i
		JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord) ON true
		LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = $1 AND i.indisunique AND i.indpred IS NULL
		  AND i.indimmediate AND i.indisvalid AND k.ord <= i.indnkeyatts
		GROUP BY i.indexrelid, i.indnkeyatts`, *oid)
	if err != nil {
		return TableInfo{}, fmt.Errorf("pginspect: unique indexes of %q: %w", table, err)
	}
	defer idxRows.Close()
	for idxRows.Next() {
		var nkey int
		var names []string
		if err := idxRows.Scan(&nkey, &names); err != nil {
			return TableInfo{}, fmt.Errorf("pginspect: unique indexes of %q: %w", table, err)
		}
		if len(names) == nkey { // no expression key columns
			info.Uniques = append(info.Uniques, names)
		}
	}
	if err := idxRows.Err(); err != nil {
		return TableInfo{}, fmt.Errorf("pginspect: unique indexes of %q: %w", table, err)
	}

	return info, nil
}

// HasUniqueOn reports whether the table has a plain unique index or
// constraint (per Uniques' definition) covering exactly cols, in any order.
func (t TableInfo) HasUniqueOn(cols ...string) bool {
	want := slices.Clone(cols)
	slices.Sort(want)
	for _, u := range t.Uniques {
		got := slices.Clone(u)
		slices.Sort(got)
		if slices.Equal(got, want) {
			return true
		}
	}
	return false
}
