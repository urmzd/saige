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

// ErrUnsupportedServer reports a PostgreSQL server older than
// MinServerVersionNum. saige needs PostgreSQL 18 or later.
var ErrUnsupportedServer = errors.New("postgres: unsupported server version")

// ErrExtensionUnavailable reports that a required extension (pgvector or
// ParadeDB pg_search) is not installed on the server, or could not be
// created in the database.
var ErrExtensionUnavailable = errors.New("postgres: required extension unavailable")

// MinServerVersionNum is the lowest supported server_version_num:
// PostgreSQL 18.0.
const MinServerVersionNum = 180000

// requiredExtension is an extension RunMigrations creates before any table.
type requiredExtension struct {
	name string
	hint string
}

// requiredExtensions are created in order. pgvector backs every embedding
// column and pg_search backs the BM25 index on rag_variant.text.
var requiredExtensions = []requiredExtension{
	{name: "vector", hint: "install pgvector (https://github.com/pgvector/pgvector)"},
	{name: "pg_search", hint: "install ParadeDB pg_search (https://github.com/paradedb/paradedb), for example with the paradedb/paradedb image"},
}

// migrationLockKey names the session advisory lock that serializes
// RunMigrations across processes sharing one database.
const migrationLockKey = "saige.migrations"

const defaultEmbeddingDim = 768

// RunMigrations creates all tables and indexes idempotently.
//
// It first checks that the server is PostgreSQL 18 or later
// (ErrUnsupportedServer) and that the vector and pg_search extensions are
// installed (ErrExtensionUnavailable), then creates them if needed.
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

	if err := checkServer(ctx, conn); err != nil {
		return err
	}
	if err := ensureExtensions(ctx, conn); err != nil {
		return err
	}

	for _, stmt := range strings.Split(renderTemplate(migrationsTmpl, opts), "---") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration %q: %w", stmt[:min(len(stmt), 80)], err)
		}
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

// checkServer rejects servers older than MinServerVersionNum.
func checkServer(ctx context.Context, conn *pgx.Conn) error {
	var (
		num     int
		version string
	)
	err := conn.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int, current_setting('server_version')`,
	).Scan(&num, &version)
	if err != nil {
		return fmt.Errorf("migration: read server version: %w", err)
	}
	return checkServerVersion(num, version)
}

// checkServerVersion reports ErrUnsupportedServer when num, a
// server_version_num value, is below MinServerVersionNum.
func checkServerVersion(num int, version string) error {
	if num >= MinServerVersionNum {
		return nil
	}
	return fmt.Errorf("%w: server is PostgreSQL %s (server_version_num %d); saige requires PostgreSQL 18 or later",
		ErrUnsupportedServer, version, num)
}

// ensureExtensions creates every required extension, failing with
// ErrExtensionUnavailable when one is not installed on the server or cannot
// be created.
func ensureExtensions(ctx context.Context, conn *pgx.Conn) error {
	for _, ext := range requiredExtensions {
		var available bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = $1)`, ext.name,
		).Scan(&available)
		if err != nil {
			return fmt.Errorf("migration: look up extension %s: %w", ext.name, err)
		}
		if !available {
			return fmt.Errorf("%w: %s is not installed on the server; %s", ErrExtensionUnavailable, ext.name, ext.hint)
		}
		// The name comes from requiredExtensions, never from input.
		if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+ext.name); err != nil {
			return fmt.Errorf("%w: create extension %s: %w", ErrExtensionUnavailable, ext.name, err)
		}
	}
	return nil
}
