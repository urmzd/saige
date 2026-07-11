package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/urmzd/saige/rag/types"
)

// CreateDocument inserts a document together with all its sections and
// variants in a single transaction: a mid-write failure rolls back the entire
// document, never leaving partial state behind.
func (s *Store) CreateDocument(ctx context.Context, doc *types.Document) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return insertDocumentTree(ctx, tx, doc)
	})
}

// ReplaceDocument atomically deletes the document identified by oldUUID and
// inserts doc in the same transaction, implementing types.DocumentReplacer.
// If any stage fails, the old document survives untouched. The delete runs
// first inside the transaction so re-ingesting content with the same
// fingerprint does not trip the unique fingerprint index.
func (s *Store) ReplaceDocument(ctx context.Context, oldUUID string, doc *types.Document) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, documentDeleteSQL, oldUUID); err != nil {
			return fmt.Errorf("delete old document: %w", err)
		}
		return insertDocumentTree(ctx, tx, doc)
	})
}

// insertDocumentTree writes the document row plus all sections and variants
// using the given transaction.
func insertDocumentTree(ctx context.Context, tx pgx.Tx, doc *types.Document) error {
	var docID int64
	err := tx.QueryRow(ctx, documentCreateSQL,
		doc.UUID, doc.SourceURI, doc.Fingerprint, doc.Title,
		encodeMetadata(doc.Metadata), doc.CreatedAt, doc.UpdatedAt,
	).Scan(&docID)
	if err != nil {
		return fmt.Errorf("create document: %w", err)
	}

	for i := range doc.Sections {
		sec := &doc.Sections[i]
		var secID int64
		err := tx.QueryRow(ctx, sectionCreateSQL,
			sec.UUID, docID, sec.Index, sec.Heading,
		).Scan(&secID)
		if err != nil {
			return fmt.Errorf("create section: %w", err)
		}
		for j := range sec.Variants {
			if err := insertVariant(ctx, tx, secID, &sec.Variants[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetDocument retrieves a document with all its sections and variants.
func (s *Store) GetDocument(ctx context.Context, uuid string) (*types.Document, error) {
	var doc types.Document
	var metaBytes []byte
	err := s.pool.QueryRow(ctx, documentGetSQL, uuid).Scan(
		&doc.UUID, &doc.SourceURI, &doc.Fingerprint, &doc.Title,
		&metaBytes, &doc.CreatedAt, &doc.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, types.ErrDocumentNotFound
		}
		return nil, err
	}
	doc.Metadata = decodeMetadata(metaBytes)

	sections, err := s.GetSections(ctx, uuid)
	if err != nil {
		return nil, err
	}
	doc.Sections = sections

	return &doc, nil
}

// FindByFingerprint finds a document by content fingerprint.
func (s *Store) FindByFingerprint(ctx context.Context, fingerprint string) (*types.Document, error) {
	var docUUID string
	err := s.pool.QueryRow(ctx, documentFindFingerprintSQL, fingerprint).Scan(&docUUID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, types.ErrDocumentNotFound
		}
		return nil, err
	}
	return s.GetDocument(ctx, docUUID)
}

// DeleteDocument removes a document and all its sections/variants via CASCADE.
func (s *Store) DeleteDocument(ctx context.Context, uuid string) error {
	_, err := s.pool.Exec(ctx, documentDeleteSQL, uuid)
	return err
}

// StoreOriginal stores the original raw bytes for a document.
func (s *Store) StoreOriginal(ctx context.Context, documentUUID string, data []byte) error {
	docID, err := s.documentID(ctx, documentUUID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, originalUpsertSQL, docID, data)
	return err
}

// GetOriginal retrieves the original raw bytes for a document.
func (s *Store) GetOriginal(ctx context.Context, documentUUID string) ([]byte, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, originalGetSQL, documentUUID).Scan(&data)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, types.ErrDocumentNotFound
		}
		return nil, err
	}
	return data, nil
}

// documentID resolves a document UUID to its internal BIGSERIAL id.
func (s *Store) documentID(ctx context.Context, docUUID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, documentIDSQL, docUUID).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, types.ErrDocumentNotFound
		}
		return 0, err
	}
	return id, nil
}
