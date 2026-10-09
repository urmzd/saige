package pgstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/rag/types"
)

// searchProjectionSQL is the column list and joins shared by vector and
// keyword search; scanSearchHits reads rows in this order, with the score
// after the document time. Its verbs are the score expression, extra
// columns after the score (empty, or highlight columns starting with a
// comma), the match condition, and the scope placeholder.
const searchProjectionSQL = `SELECT v.uuid, v.content_type, v.mime_type, v.data, v.text, v.embedding, v.metadata,
	                 s.uuid, s.heading, s.idx,
	                 d.uuid, d.title, d.source_uri, d.metadata,
	                 ` + docTimeSQL + ` AS doc_ts,
	                 %s AS score%s
	          FROM rag_variant v
	          JOIN rag_section s ON s.id = v.section_id
	          JOIN rag_document d ON d.id = s.document_id
	          WHERE %s AND d.scope = %s`

// vectorScoreSQL is the cosine similarity between a variant and the query
// embedding in $1.
const vectorScoreSQL = `1 - (v.embedding <=> $1)`

// searchBaseSQL is the vector-search projection and joins; buildSearchSQL
// appends filter clauses, ordering, and the limit.
var searchBaseSQL = fmt.Sprintf(searchProjectionSQL, vectorScoreSQL, "", `v.embedding IS NOT NULL`, "$2")

// docTimeSQL is the document's effective time, matching
// types.Document.EffectiveTime.
const docTimeSQL = `COALESCE(d.source_modified_at, d.updated_at, d.created_at)`

// mergedMetadataSQL is the document metadata overlaid with variant metadata,
// matching the merge semantics used by memstore (variant keys win).
const mergedMetadataSQL = `(COALESCE(d.metadata, '{}'::jsonb) || COALESCE(v.metadata, '{}'::jsonb))`

// buildSearchSQL builds the full vector-search query and its argument list.
// The embedding is always $1 and the scope is always $2: every search is an
// exact match on one scope, the empty string being the default scope. The
// time range and metadata filters are pushed down into the WHERE clause, with per-operator semantics identical to memstore's matchFilters.
// Whether the top `limit` qualifying rows come back depends on the scan; see
// SearchByEmbedding:
//
//   - FilterEq: key must exist and equal the value (missing key -> excluded).
//   - FilterNeq: row excluded only when the key exists and equals the value.
//   - FilterContains: key must exist and contain the value as a substring.
//
// Unknown filter operators are ignored, matching the previous in-Go behavior.
func buildSearchSQL(embedding any, opts *types.SearchOptions, limit int) (string, []any) {
	return buildScopedSQL(searchBaseSQL, vectorScoreSQL, " ORDER BY v.embedding <=> $1", []any{embedding, scopeOf(opts)}, opts, limit)
}

// scopeOf returns the scope opts searches, the default scope for nil opts.
func scopeOf(opts *types.SearchOptions) string {
	if opts == nil {
		return ""
	}
	return opts.Scope
}

// buildScopedSQL appends the filters shared by vector and keyword search to
// base, whose placeholders are numbered by args, and returns the query and
// its full argument list. scoreExpr is the score that MinScore compares
// against, and orderBy ranks the rows before the limit.
func buildScopedSQL(base, scoreExpr, orderBy string, args []any, opts *types.SearchOptions, limit int) (string, []any) {
	query := base

	if opts != nil {
		if !opts.Since.IsZero() {
			query += fmt.Sprintf(" AND %s >= $%d", docTimeSQL, len(args)+1)
			args = append(args, opts.Since)
		}
		if !opts.Until.IsZero() {
			query += fmt.Sprintf(" AND %s < $%d", docTimeSQL, len(args)+1)
			args = append(args, opts.Until)
		}

		if len(opts.ContentTypes) > 0 {
			cts := make([]string, len(opts.ContentTypes))
			for i, ct := range opts.ContentTypes {
				cts[i] = string(ct)
			}
			query += fmt.Sprintf(" AND v.content_type = ANY($%d)", len(args)+1)
			args = append(args, cts)
		}

		if opts.MinScore > 0 {
			query += fmt.Sprintf(" AND %s >= $%d", scoreExpr, len(args)+1)
			args = append(args, opts.MinScore)
		}

		for _, f := range opts.MetadataFilters {
			switch f.Op {
			case types.FilterEq:
				query += fmt.Sprintf(" AND %s ->> $%d = $%d", mergedMetadataSQL, len(args)+1, len(args)+2)
				args = append(args, f.Key, f.Value)
			case types.FilterNeq:
				// IS DISTINCT FROM keeps rows where the key is absent (NULL),
				// matching matchFilters' "absent key passes neq" semantics.
				query += fmt.Sprintf(" AND (%s ->> $%d) IS DISTINCT FROM $%d", mergedMetadataSQL, len(args)+1, len(args)+2)
				args = append(args, f.Key, f.Value)
			case types.FilterContains:
				// position() avoids LIKE wildcard escaping and mirrors
				// strings.Contains (empty needle matches any present key).
				query += fmt.Sprintf(" AND position($%d IN %s ->> $%d) > 0", len(args)+2, mergedMetadataSQL, len(args)+1)
				args = append(args, f.Key, f.Value)
			}
		}
	}

	query += fmt.Sprintf("%s LIMIT $%d", orderBy, len(args)+1)
	args = append(args, limit)

	return query, args
}

// SearchByEmbedding performs HNSW vector similarity search over variants.
// Content-type, min-score, and metadata filters are all evaluated in SQL.
//
// An HNSW index scan only examines hnsw.ef_search candidates, so filters
// applied to that candidate list can leave fewer than `limit` rows. For
// queries with content-type or metadata filters the store therefore enables
// pgvector iterative scans (see IterativeScan), which keep scanning until
// enough rows qualify. MinScore alone does not enable them: a distance
// threshold that fewer than `limit` rows pass would keep an iterative scan
// running to hnsw.max_scan_tuples on every query, and rows past the threshold
// could not qualify anyway because the scan visits them in distance order.
// The settings
// are applied with set_config(..., true) inside a transaction, so they never
// leak to other users of a pooled connection. With IterativeScanOff, or on
// pgvector older than 0.8.0 in the default mode, a selective filter can still
// return fewer rows than requested when the planner chooses the HNSW index.
func (s *Store) SearchByEmbedding(ctx context.Context, embedding []float32, opts *types.SearchOptions) ([]types.SearchHit, error) {
	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}

	query, args := buildSearchSQL(pgvector.NewVector(embedding), opts, limit)

	settings, mode, err := s.searchSettings(ctx, hasSearchFilters(opts))
	if err != nil {
		return nil, err
	}
	if len(settings) == 0 {
		return scanSearchHits(ctx, s.pool, query, args, false)
	}

	var results []types.SearchHit
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, st := range settings {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, st.name, st.value); err != nil {
				return fmt.Errorf("pgstore: set %s=%s (iterative scans and max_scan_tuples need pgvector 0.8.0 or later): %w",
					st.name, st.value, err)
			}
		}
		var err error
		results, err = scanSearchHits(ctx, tx, query, args, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	if mode == IterativeScanRelaxed {
		// relaxed_order may return rows slightly out of distance order.
		sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	}
	return results, nil
}

// hasSearchFilters reports whether opts adds a WHERE clause that can reject
// HNSW candidates out of distance order, so more candidates may still
// qualify. A named scope counts: other scopes' nearer vectors would
// otherwise use up the candidate list. The default scope does not, so a
// store shared by several scopes should give every one of them a name. MinScore is excluded: it rejects every candidate past a distance
// cutoff, so scanning further cannot find more rows.
func hasSearchFilters(opts *types.SearchOptions) bool {
	if opts == nil {
		return false
	}
	return len(opts.ContentTypes) > 0 || len(opts.MetadataFilters) > 0 ||
		opts.Scope != "" || !opts.Since.IsZero() || !opts.Until.IsZero()
}

// querier is the subset of pgxpool.Pool and pgx.Tx that search needs.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// scanSearchHits runs query and reads its rows. With highlight, each row
// also carries a snippet and snippet byte positions after the score.
func scanSearchHits(ctx context.Context, q querier, query string, args []any, highlight bool) ([]types.SearchHit, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []types.SearchHit
	for rows.Next() {
		var (
			hit   types.SearchHit
			ct    string
			vEmb  *pgvector.Vector // NULL for variants a keyword search finds without an embedding
			vMeta []byte
			dMeta []byte
		)
		dest := []any{
			&hit.Variant.UUID, &ct, &hit.Variant.MIMEType,
			&hit.Variant.Data, &hit.Variant.Text, &vEmb, &vMeta,
			&hit.Provenance.SectionUUID, &hit.Provenance.SectionHeading, &hit.Provenance.SectionIndex,
			&hit.Provenance.DocumentUUID, &hit.Provenance.DocumentTitle, &hit.Provenance.SourceURI,
			&dMeta, &hit.Timestamp, &hit.Score,
		}
		var (
			snippet   *string
			positions [][]int32
		)
		if highlight {
			dest = append(dest, &snippet, &positions)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if highlight {
			hit.Highlight = newHighlight(snippet, positions, len(hit.Variant.Text))
		}

		hit.Variant.ContentType = types.ContentType(ct)
		if vEmb != nil {
			hit.Variant.Embedding = vEmb.Slice()
		}
		hit.Variant.Metadata = decodeMetadata(vMeta)

		results = append(results, hit)
	}

	return results, rows.Err()
}
