package pgstore

import (
	"context"
	"fmt"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/rag/types"
)

// searchBaseSQL is the vector-search projection and joins; buildSearchSQL
// appends filter clauses, ordering, and the limit.
const searchBaseSQL = `SELECT v.uuid, v.content_type, v.mime_type, v.data, v.text, v.embedding, v.metadata,
	                 s.uuid, s.heading, s.idx,
	                 d.uuid, d.title, d.source_uri, d.metadata,
	                 COALESCE(d.updated_at, d.created_at) AS doc_ts,
	                 1 - (v.embedding <=> $1) AS score
	          FROM rag_variant v
	          JOIN rag_section s ON s.id = v.section_id
	          JOIN rag_document d ON d.id = s.document_id
	          WHERE v.embedding IS NOT NULL`

// mergedMetadataSQL is the document metadata overlaid with variant metadata,
// matching the merge semantics used by memstore (variant keys win).
const mergedMetadataSQL = `(COALESCE(d.metadata, '{}'::jsonb) || COALESCE(v.metadata, '{}'::jsonb))`

// buildSearchSQL builds the full vector-search query and its argument list.
// The embedding is always $1. Metadata filters are pushed down into the WHERE
// clause so the database returns the top `limit` qualifying rows directly,
// with per-operator semantics identical to memstore's matchFilters:
//
//   - FilterEq: key must exist and equal the value (missing key -> excluded).
//   - FilterNeq: row excluded only when the key exists and equals the value.
//   - FilterContains: key must exist and contain the value as a substring.
//
// Unknown filter operators are ignored, matching the previous in-Go behavior.
func buildSearchSQL(embedding any, opts *types.SearchOptions, limit int) (string, []any) {
	query := searchBaseSQL
	args := []any{embedding}

	if opts != nil {
		if len(opts.ContentTypes) > 0 {
			cts := make([]string, len(opts.ContentTypes))
			for i, ct := range opts.ContentTypes {
				cts[i] = string(ct)
			}
			query += fmt.Sprintf(" AND v.content_type = ANY($%d)", len(args)+1)
			args = append(args, cts)
		}

		if opts.MinScore > 0 {
			query += fmt.Sprintf(" AND 1 - (v.embedding <=> $1) >= $%d", len(args)+1)
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

	query += fmt.Sprintf(" ORDER BY v.embedding <=> $1 LIMIT $%d", len(args)+1)
	args = append(args, limit)

	return query, args
}

// SearchByEmbedding performs HNSW vector similarity search over variants.
// Content-type, min-score, and metadata filters are all evaluated in SQL, so
// restrictive filters still return up to `limit` qualifying rows.
func (s *Store) SearchByEmbedding(ctx context.Context, embedding []float32, opts *types.SearchOptions) ([]types.SearchHit, error) {
	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}

	query, args := buildSearchSQL(pgvector.NewVector(embedding), opts, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []types.SearchHit
	for rows.Next() {
		var (
			hit   types.SearchHit
			ct    string
			vEmb  pgvector.Vector
			vMeta []byte
			dMeta []byte
		)
		if err := rows.Scan(
			&hit.Variant.UUID, &ct, &hit.Variant.MIMEType,
			&hit.Variant.Data, &hit.Variant.Text, &vEmb, &vMeta,
			&hit.Provenance.SectionUUID, &hit.Provenance.SectionHeading, &hit.Provenance.SectionIndex,
			&hit.Provenance.DocumentUUID, &hit.Provenance.DocumentTitle, &hit.Provenance.SourceURI,
			&dMeta, &hit.Timestamp, &hit.Score,
		); err != nil {
			return nil, err
		}

		hit.Variant.ContentType = types.ContentType(ct)
		hit.Variant.Embedding = vEmb.Slice()
		hit.Variant.Metadata = decodeMetadata(vMeta)

		results = append(results, hit)
	}

	return results, rows.Err()
}
