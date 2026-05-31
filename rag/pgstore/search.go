package pgstore

import (
	"context"
	"fmt"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/rag/types"
)

// SearchByEmbedding performs HNSW vector similarity search over variants.
func (s *Store) SearchByEmbedding(ctx context.Context, embedding []float32, opts *types.SearchOptions) ([]types.SearchHit, error) {
	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}

	// Over-fetch to allow for metadata filtering in Go.
	fetchLimit := limit
	hasFilters := opts != nil && len(opts.MetadataFilters) > 0
	if hasFilters {
		fetchLimit = limit * 3
	}

	emb := pgvector.NewVector(embedding)

	query := `SELECT v.uuid, v.content_type, v.mime_type, v.data, v.text, v.embedding, v.metadata,
	                 s.uuid, s.heading, s.idx,
	                 d.uuid, d.title, d.source_uri, d.metadata,
	                 COALESCE(d.updated_at, d.created_at) AS doc_ts,
	                 1 - (v.embedding <=> $1) AS score
	          FROM rag_variant v
	          JOIN rag_section s ON s.id = v.section_id
	          JOIN rag_document d ON d.id = s.document_id
	          WHERE v.embedding IS NOT NULL`

	args := []any{emb}
	argIdx := 2

	if opts != nil && len(opts.ContentTypes) > 0 {
		cts := make([]string, len(opts.ContentTypes))
		for i, ct := range opts.ContentTypes {
			cts[i] = string(ct)
		}
		query += fmt.Sprintf(" AND v.content_type = ANY($%d)", argIdx)
		args = append(args, cts)
		argIdx++
	}

	if opts != nil && opts.MinScore > 0 {
		query += fmt.Sprintf(" AND 1 - (v.embedding <=> $1) >= $%d", argIdx)
		args = append(args, opts.MinScore)
		argIdx++
	}

	query += fmt.Sprintf(" ORDER BY v.embedding <=> $1 LIMIT $%d", argIdx)
	args = append(args, fetchLimit)

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

		// Apply metadata filters in Go (merged doc + variant metadata, matching memstore behavior).
		if hasFilters {
			merged := mergeMetadata(decodeMetadata(dMeta), hit.Variant.Metadata)
			if !matchFilters(merged, opts.MetadataFilters) {
				continue
			}
		}

		results = append(results, hit)
		if len(results) >= limit {
			break
		}
	}

	return results, rows.Err()
}
