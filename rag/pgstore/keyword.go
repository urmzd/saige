package pgstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/urmzd/saige/rag/internal/textclean"
	"github.com/urmzd/saige/rag/types"
)

var (
	_ types.KeywordSearcher = (*Store)(nil)
	_ types.Retriever       = (*KeywordRetriever)(nil)
	_ types.Named           = (*KeywordRetriever)(nil)
)

// keywordScoreSQL is the BM25 score pg_search assigns to a variant matched
// by the @@@ condition in keywordBaseSQL. It is a real; the projection casts
// it to float8, but WHERE and ORDER BY use it uncast because pg_search
// 0.26 rejects a cast score in a WHERE clause.
const keywordScoreSQL = `pdb.score(v.id)`

// keywordBaseSQL is the keyword-search projection and joins. $1 is the query
// text, matched against the BM25 index on rag_variant.text
// (idx_rag_variant_bm25). pdb.match tokenizes the text with the field's
// tokenizer and matches any term, so query syntax characters in user input
// are plain text, never query-parser operators.
var keywordBaseSQL = fmt.Sprintf(searchProjectionSQL, keywordScoreSQL+`::float8`, `v.text @@@ pdb.match($1)`)

// buildKeywordSQL builds the full keyword-search query and its argument
// list. The query text is always $1 and the scope is always $2. Scope, time
// range, content types, and metadata filters are the same clauses vector
// search uses (see buildSearchSQL); MinScore compares against the BM25
// score. Ties are broken by variant UUID so results are deterministic.
func buildKeywordSQL(text string, opts *types.SearchOptions, limit int) (string, []any) {
	return buildScopedSQL(keywordBaseSQL, keywordScoreSQL,
		" ORDER BY "+keywordScoreSQL+" DESC, v.uuid", text, opts, limit)
}

// SearchByKeyword performs BM25 full-text search over variant text with
// ParadeDB pg_search. It applies the same scope, time-range, content-type,
// and metadata filters as SearchByEmbedding, in SQL, before the limit.
// Scores are BM25 scores, not cosine similarities, so a MinScore tuned for
// vector search does not carry over.
//
// The BM25 index lives in Postgres next to the rows it indexes, so inserts,
// ReplaceDocument, and DeleteDocument update it in the same transaction and
// every process sees the same index. An empty or blank query returns no hits.
func (s *Store) SearchByKeyword(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}
	sql, args := buildKeywordSQL(textclean.String(query), opts, limit)

	var results []types.SearchHit
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The query joins the indexed table to unindexed ones, which
		// pg_search reports as a planner warning on every search. The plan
		// is the intended one, so silence the warning for this transaction.
		if _, err := tx.Exec(ctx, `SELECT set_config('paradedb.planner_warnings', 'off', true)`); err != nil {
			return fmt.Errorf("pgstore: configure keyword search: %w", err)
		}
		var err error
		results, err = scanSearchHits(ctx, tx, sql, args)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: keyword search (requires the pg_search extension): %w", err)
	}
	return results, nil
}

// KeywordRetriever is a types.Retriever that runs BM25 keyword search in
// Postgres through Store.SearchByKeyword. Pair it with a vector retriever
// for hybrid search; the pipeline fuses both with its configured Fuser.
//
// Unlike bm25retriever, it keeps no in-memory index: it needs no Index,
// Remove, or RebuildIndex calls, and a new process searches documents that
// another process ingested.
type KeywordRetriever struct {
	searcher types.KeywordSearcher
}

// NewKeywordRetriever returns a BM25 retriever over a store's keyword
// search, such as a *Store.
func NewKeywordRetriever(searcher types.KeywordSearcher) *KeywordRetriever {
	return &KeywordRetriever{searcher: searcher}
}

// Retrieve returns the variants that best match query by BM25 score.
func (r *KeywordRetriever) Retrieve(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	return r.searcher.SearchByKeyword(ctx, query, opts)
}

// Name reports "bm25", the same name bm25retriever uses, so fusion weights
// keyed by name apply to either backend.
func (r *KeywordRetriever) Name() string { return "bm25" }
