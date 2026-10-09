package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationOptions configures schema migration.
type MigrationOptions struct {
	// KGEmbeddingDim is the kg_entity.embedding dimension. Zero creates new
	// tables with 768 and skips the dimension check for existing ones.
	KGEmbeddingDim int
	// RAGEmbeddingDim is the rag_variant.embedding dimension. Zero creates new
	// tables with 768 and skips the dimension check for existing ones.
	RAGEmbeddingDim int
}

// ErrEmbeddingDimMismatch reports that an existing vector column has a
// different dimension than MigrationOptions requests. CREATE TABLE IF NOT
// EXISTS keeps the old column, so inserts with the new dimension would fail;
// re-embed into a fresh schema or pass the existing dimension.
var ErrEmbeddingDimMismatch = errors.New("postgres: embedding dimension mismatch")

// migrationLockKey names the session advisory lock that serializes
// RunMigrations across processes sharing one database.
const migrationLockKey = "saige.migrations"

const defaultEmbeddingDim = 768

// RunMigrations creates all tables and indexes idempotently.
//
// It holds a session advisory lock on one connection for the whole run, so
// replicas that start together migrate one at a time instead of racing on
// catalog rows. It stops at the first failing statement, so later backfills
// never run against a partial schema. When a dimension is set explicitly,
// it then checks the existing vector column and returns
// ErrEmbeddingDimMismatch if they differ.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, opts MigrationOptions) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migration: acquire connection: %w", err)
	}
	defer conn.Release()
	return runMigrations(ctx, conn.Conn(), opts)
}

func runMigrations(ctx context.Context, conn *pgx.Conn, opts MigrationOptions) (err error) {
	checkKG, checkRAG := opts.KGEmbeddingDim > 0, opts.RAGEmbeddingDim > 0
	if !checkKG {
		opts.KGEmbeddingDim = defaultEmbeddingDim
	}
	if !checkRAG {
		opts.RAGEmbeddingDim = defaultEmbeddingDim
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, migrationLockKey); err != nil {
		return fmt.Errorf("migration: acquire lock: %w", err)
	}
	defer func() {
		// Unlock even when ctx is done: a session lock outlives the statement
		// and would otherwise stay held by a pooled connection.
		_, unlockErr := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, migrationLockKey)
		if unlockErr != nil && err == nil {
			err = fmt.Errorf("migration: release lock: %w", unlockErr)
		}
	}()

	for _, stmt := range strings.Split(renderTemplate(migrationsTmpl, opts), "---") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration %q: %w", stmt[:min(len(stmt), 80)], err)
		}
	}
	// Notifier and CacheStore tables live in their own script.
	if err := execScript(ctx, conn, notifySQL); err != nil {
		return err
	}

	if checkKG {
		if err := checkVectorDim(ctx, conn, "kg_entity", "embedding", opts.KGEmbeddingDim); err != nil {
			return err
		}
	}
	if checkRAG {
		if err := checkVectorDim(ctx, conn, "rag_variant", "embedding", opts.RAGEmbeddingDim); err != nil {
			return err
		}
	}
	return nil
}

// checkVectorDim compares a pgvector column's declared dimension, which
// pgvector stores as the attribute's type modifier, with want.
func checkVectorDim(ctx context.Context, conn *pgx.Conn, table, column string, want int) error {
	var got int
	err := conn.QueryRow(ctx,
		`SELECT atttypmod FROM pg_attribute
		 WHERE attrelid = to_regclass($1) AND attname = $2 AND NOT attisdropped`,
		table, column,
	).Scan(&got)
	if err != nil {
		return fmt.Errorf("migration: read %s.%s dimension: %w", table, column, err)
	}
	if got != want {
		return fmt.Errorf("%w: %s.%s is vector(%d), options request vector(%d)",
			ErrEmbeddingDimMismatch, table, column, got, want)
	}
	return nil
}

// execScript runs each "---"-separated statement of script in order and
// stops at the first failure.
func execScript(ctx context.Context, conn *pgx.Conn, script string) error {
	for _, stmt := range strings.Split(script, "---") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration %q: %w", stmt[:min(len(stmt), 80)], err)
		}
	}
	return nil
}
