// Package pgkit is the Postgres plumbing the stack shares instead of each
// component hand-rolling it: Apply runs a component's schema statements once
// per database under an advisory lock, and Listener carries LISTEN/NOTIFY for
// a whole process over one dedicated connection.
//
// Boundary: pgkit never decides what a schema is. Apply runs exactly the
// statements its caller passes, when its caller calls it. durable's own
// adapters never call it: pgledger and pgqueue validate tables and never
// migrate them, and their RecommendedDDL strings are suggestions an
// application may choose to hand to Apply, or to its own migration tooling.
//
// Both halves work on a *pgxpool.Pool the caller opened and closes. Nothing
// here reads configuration: the pool, the names and the statements all come
// from code.
package pgkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applyLockSpace prefixes every Apply lock name, so an Apply lock never
// collides with an advisory lock some other code derives from the same text.
const applyLockSpace = "pgkit.apply:"

// Beginner is what Apply runs on. A *pgxpool.Pool and a *pgx.Conn begin a
// transaction; a pgx.Tx begins a savepoint, so Apply then joins the caller's
// transaction and its lock is held until that transaction ends.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Apply runs steps in order, in one transaction, holding a transaction-scoped
// advisory lock derived from name.
//
// Processes that start together and call Apply with the same name run it one
// at a time. That is the point: CREATE TABLE IF NOT EXISTS is not safe
// against itself (two sessions can both pass the existence check and one then
// fails on a catalog unique index), and the lock turns the race into a wait.
// The lock is released when the transaction commits or rolls back, so a
// process that dies mid-migration never leaves it held. Different names do
// not wait on each other; use one name per set of objects that must be
// created together.
//
// Steps must be idempotent (CREATE ... IF NOT EXISTS, ADD COLUMN IF NOT
// EXISTS, CREATE OR REPLACE): Apply keeps no version table and runs every
// step on every call, which is what makes it safe to call from every replica
// at startup. A step may hold several statements separated by semicolons: it
// is sent without parameters, over the simple protocol. Blank steps are
// skipped. A failing step rolls back every step before it and the error
// names its index. Statements Postgres refuses inside a transaction block,
// such as CREATE INDEX CONCURRENTLY or VACUUM, cannot be steps.
func Apply(ctx context.Context, db Beginner, name string, steps ...string) error {
	if name == "" {
		return errors.New("pgkit: apply: name is required")
	}
	if isNil(db) {
		return fmt.Errorf("pgkit: apply %q: no database", name)
	}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, applyLockSpace+name); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		for i, step := range steps {
			if strings.TrimSpace(step) == "" {
				continue
			}
			if _, err := tx.Exec(ctx, step); err != nil {
				return fmt.Errorf("step %d (%s): %w", i, preview(step), err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("pgkit: apply %q: %w", name, err)
	}
	return nil
}

// isNil reports a missing database: a nil interface, or a typed nil of the
// two concrete kinds callers pass, which would otherwise panic inside Begin.
func isNil(db Beginner) bool {
	switch v := db.(type) {
	case nil:
		return true
	case *pgxpool.Pool:
		return v == nil
	case *pgx.Conn:
		return v == nil
	}
	return false
}

// preview is the start of a step, on one line, for error messages.
func preview(step string) string {
	s := strings.Join(strings.Fields(step), " ")
	const limit = 60
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
