package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/rag/types"
)

// execer abstracts pgxpool.Pool and pgx.Tx for shared insert helpers.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// CreateVariant inserts a new content variant for a section.
func (s *Store) CreateVariant(ctx context.Context, variant *types.ContentVariant) error {
	secID, err := s.sectionID(ctx, variant.SectionUUID)
	if err != nil {
		return err
	}
	return insertVariant(ctx, s.pool, secID, variant)
}

// insertVariant writes a single variant row using the given executor.
func insertVariant(ctx context.Context, db execer, sectionID int64, variant *types.ContentVariant) error {
	var emb *pgvector.Vector
	if variant.Embedding != nil {
		v := pgvector.NewVector(variant.Embedding)
		emb = &v
	}

	_, err := db.Exec(ctx, variantCreateSQL,
		variant.UUID, sectionID, string(variant.ContentType), variant.MIMEType,
		variant.Data, variant.Text, emb, encodeMetadata(variant.Metadata),
	)
	if err != nil {
		return fmt.Errorf("create variant: %w", err)
	}
	return nil
}

// UpdateVariantEmbedding updates the embedding for an existing variant.
func (s *Store) UpdateVariantEmbedding(ctx context.Context, variantUUID string, embedding []float32) error {
	emb := pgvector.NewVector(embedding)
	tag, err := s.pool.Exec(ctx, variantUpdateEmbeddingSQL, emb, variantUUID)
	if err != nil {
		return fmt.Errorf("update variant embedding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return types.ErrDocumentNotFound
	}
	return nil
}

// GetVariant retrieves a variant with its provenance information.
func (s *Store) GetVariant(ctx context.Context, variantUUID string) (*types.ContentVariant, *types.Provenance, error) {
	var (
		v     types.ContentVariant
		prov  types.Provenance
		ct    string
		emb   *pgvector.Vector
		vMeta []byte
	)
	err := s.pool.QueryRow(ctx, variantGetSQL, variantUUID).Scan(
		&v.UUID, &ct, &v.MIMEType, &v.Data, &v.Text, &emb, &vMeta,
		&prov.SectionUUID, &prov.SectionHeading, &prov.SectionIndex,
		&prov.DocumentUUID, &prov.DocumentTitle, &prov.SourceURI,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil, types.ErrVariantNotFound
		}
		return nil, nil, err
	}

	v.ContentType = types.ContentType(ct)
	v.SectionUUID = prov.SectionUUID
	if emb != nil {
		v.Embedding = emb.Slice()
	}
	v.Metadata = decodeMetadata(vMeta)

	return &v, &prov, nil
}

// GetVariantRecord retrieves a variant with its provenance and the owning
// document's metadata and timestamp, implementing types.VariantRecordGetter.
func (s *Store) GetVariantRecord(ctx context.Context, variantUUID string) (*types.VariantRecord, error) {
	var (
		rec   types.VariantRecord
		ct    string
		emb   *pgvector.Vector
		vMeta []byte
		dMeta []byte
	)
	v := &rec.Variant
	prov := &rec.Provenance
	err := s.pool.QueryRow(ctx, variantGetRecordSQL, variantUUID).Scan(
		&v.UUID, &ct, &v.MIMEType, &v.Data, &v.Text, &emb, &vMeta,
		&prov.SectionUUID, &prov.SectionHeading, &prov.SectionIndex,
		&prov.DocumentUUID, &prov.DocumentTitle, &prov.SourceURI,
		&dMeta, &rec.Timestamp, &rec.Scope,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, types.ErrVariantNotFound
		}
		return nil, err
	}

	v.ContentType = types.ContentType(ct)
	v.SectionUUID = prov.SectionUUID
	if emb != nil {
		v.Embedding = emb.Slice()
	}
	v.Metadata = decodeMetadata(vMeta)
	rec.DocumentMetadata = decodeMetadata(dMeta)
	return &rec, nil
}

// GetVariantRecords retrieves the records for many variants in one query,
// implementing types.VariantRecordsGetter. UUIDs with no stored variant are
// absent from the result.
func (s *Store) GetVariantRecords(ctx context.Context, variantUUIDs []string) (map[string]*types.VariantRecord, error) {
	out := make(map[string]*types.VariantRecord, len(variantUUIDs))
	if len(variantUUIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, variantGetRecordsSQL, variantUUIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rec   types.VariantRecord
			ct    string
			emb   *pgvector.Vector
			vMeta []byte
			dMeta []byte
		)
		v := &rec.Variant
		prov := &rec.Provenance
		if err := rows.Scan(
			&v.UUID, &ct, &v.MIMEType, &v.Data, &v.Text, &emb, &vMeta,
			&prov.SectionUUID, &prov.SectionHeading, &prov.SectionIndex,
			&prov.DocumentUUID, &prov.DocumentTitle, &prov.SourceURI,
			&dMeta, &rec.Timestamp, &rec.Scope,
		); err != nil {
			return nil, err
		}
		v.ContentType = types.ContentType(ct)
		v.SectionUUID = prov.SectionUUID
		if emb != nil {
			v.Embedding = emb.Slice()
		}
		v.Metadata = decodeMetadata(vMeta)
		rec.DocumentMetadata = decodeMetadata(dMeta)
		out[v.UUID] = &rec
	}
	return out, rows.Err()
}
