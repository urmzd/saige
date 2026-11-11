package legacy

import "strings"

// Flow was called mandatum before its rename. These names are read only so
// that a database from then can be moved forward, and a run it still holds
// can be recognized and ended. docs/guides/renaming.md has the history.

// FiberPrefixBeforeRename is the prefix flow's Postgres objects had before
// the rename. Each now starts with fiber_.
const FiberPrefixBeforeRename = "mandatum_"

// FiberTurnBeforeRename is the workflow name runs were accepted under before
// the rename. A run still pending under it recorded its steps under the old
// names, which the current turn does not read, so it cannot be resumed. The
// runtime registers this name only to fail such a run with a reason, instead
// of leaving durable's worker to fail it as an unknown workflow.
const FiberTurnBeforeRename = "mandatum.turn"

// FiberTablesSQL renames each of tables (named without a prefix, "agents")
// from FiberPrefixBeforeRename to fiber_ in the current schema, with every
// index and constraint on it named with the old prefix, when the old table
// exists. It is idempotent. Table names are flow's own constants, never
// input. Each store's Migrate runs it in its own transaction, under its
// advisory lock, before its CREATE IF NOT EXISTS statements, so an upgraded
// database keeps its rows and the order stores migrate in does not matter.
//
// When the old table and the new one both exist, it raises an error naming
// both instead of choosing: a process from before the rename created the old
// one again, and whatever it wrote there would otherwise be orphaned.
func FiberTablesSQL(tables ...string) string {
	quoted := make([]string, len(tables))
	for i, t := range tables {
		quoted[i] = "'" + t + "'"
	}
	return `
DO $legacy$
DECLARE
	t text;
	r record;
	old_prefix constant text := '` + FiberPrefixBeforeRename + `';
	n constant int := length('` + FiberPrefixBeforeRename + `');
	tbl regclass;
BEGIN
	FOREACH t IN ARRAY ARRAY[` + strings.Join(quoted, ", ") + `]::text[] LOOP
		tbl := to_regclass(quote_ident(current_schema()) || '.' || quote_ident(old_prefix || t));
		CONTINUE WHEN tbl IS NULL;
		IF to_regclass(quote_ident(current_schema()) || '.' || quote_ident('fiber_' || t)) IS NOT NULL THEN
			RAISE EXCEPTION USING
				ERRCODE = 'duplicate_table',
				MESSAGE = format('legacy: both %I.%I and %I.%I exist: a process from before the rename created %I again after this database was moved forward. Stop every such process, move any rows in %I into %I, drop %I, and migrate again',
					current_schema(), old_prefix || t, current_schema(), 'fiber_' || t,
					old_prefix || t, old_prefix || t, 'fiber_' || t, old_prefix || t);
		END IF;
		FOR r IN SELECT ic.relname FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
			WHERE i.indrelid = tbl AND left(ic.relname, n) = old_prefix
		LOOP
			EXECUTE format('ALTER INDEX %I RENAME TO %I', r.relname, 'fiber_' || substr(r.relname, n + 1));
		END LOOP;
		FOR r IN SELECT con.conname FROM pg_constraint con
			WHERE con.conrelid = tbl AND left(con.conname, n) = old_prefix
		LOOP
			EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', tbl, r.conname, 'fiber_' || substr(r.conname, n + 1));
		END LOOP;
		EXECUTE format('ALTER TABLE %s RENAME TO %I', tbl, 'fiber_' || t);
	END LOOP;
END
$legacy$;
`
}
