package surrealdb

import (
	"context"
	"errors"
	"fmt"

	surrealdbgo "github.com/surrealdb/surrealdb.go"
)

// ensureSchema creates the knowledge graph tables and indexes.
func ensureSchema(ctx context.Context, db *surrealdbgo.DB) error {
	statements := []string{
		"DEFINE TABLE IF NOT EXISTS entity SCHEMAFULL",
		"DEFINE FIELD IF NOT EXISTS uuid ON entity TYPE string",
		"DEFINE FIELD IF NOT EXISTS name ON entity TYPE string",
		"DEFINE FIELD IF NOT EXISTS type ON entity TYPE string",
		"DEFINE FIELD IF NOT EXISTS summary ON entity TYPE string",
		"DEFINE FIELD IF NOT EXISTS embedding ON entity TYPE option<array<float>>",
		"DEFINE INDEX IF NOT EXISTS entity_uuid ON entity FIELDS uuid UNIQUE",
		"DEFINE INDEX IF NOT EXISTS entity_name_type ON entity FIELDS name, type UNIQUE",
		"DEFINE INDEX IF NOT EXISTS entity_embedding ON entity FIELDS embedding HNSW DIMENSION 768 DIST COSINE",
		"DEFINE ANALYZER IF NOT EXISTS entity_analyzer TOKENIZERS blank, class FILTERS snowball(english)",
		"DEFINE INDEX IF NOT EXISTS entity_name_ft ON entity FIELDS name FULLTEXT ANALYZER entity_analyzer BM25",
		"DEFINE INDEX IF NOT EXISTS entity_summary_ft ON entity FIELDS summary FULLTEXT ANALYZER entity_analyzer BM25",
		"DEFINE TABLE IF NOT EXISTS episode SCHEMAFULL",
		"DEFINE FIELD IF NOT EXISTS uuid ON episode TYPE string",
		"DEFINE FIELD IF NOT EXISTS name ON episode TYPE string",
		"DEFINE FIELD IF NOT EXISTS body ON episode TYPE string",
		"DEFINE FIELD IF NOT EXISTS source ON episode TYPE string",
		"DEFINE FIELD IF NOT EXISTS group_id ON episode TYPE string",
		"DEFINE FIELD IF NOT EXISTS created_at ON episode TYPE datetime DEFAULT time::now()",
		"DEFINE INDEX IF NOT EXISTS episode_uuid ON episode FIELDS uuid UNIQUE",
		"DEFINE TABLE IF NOT EXISTS relates_to SCHEMAFULL TYPE RELATION IN entity OUT entity",
		"DEFINE FIELD IF NOT EXISTS uuid ON relates_to TYPE string",
		"DEFINE FIELD IF NOT EXISTS type ON relates_to TYPE string",
		"DEFINE FIELD IF NOT EXISTS fact ON relates_to TYPE string",
		"DEFINE FIELD IF NOT EXISTS created_at ON relates_to TYPE datetime DEFAULT time::now()",
		"DEFINE FIELD IF NOT EXISTS valid_at ON relates_to TYPE datetime DEFAULT time::now()",
		"DEFINE FIELD IF NOT EXISTS invalid_at ON relates_to TYPE option<datetime>",
		"DEFINE INDEX IF NOT EXISTS relates_to_uuid ON relates_to FIELDS uuid UNIQUE",
		"DEFINE TABLE IF NOT EXISTS mentions SCHEMAFULL TYPE RELATION IN episode OUT entity",
	}

	var errs []error
	for _, stmt := range statements {
		if _, err := surrealdbgo.Query[any](ctx, db, stmt, nil); err != nil {
			errs = append(errs, fmt.Errorf("schema %q: %w", stmt, err))
		}
	}
	return errors.Join(errs...)
}
