package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urmzd/saige/rag/types"
)

// CreateDocument inserts a document together with all its sections and
// variants in a single transaction: a mid-write failure rolls back the entire
// document, never leaving partial state behind. It returns an error wrapping
// types.ErrDuplicateDocument when another document holds the same non-empty
// fingerprint.
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
		doc.Scope, nullableTime(doc.SourceModifiedAt),
	).Scan(&docID)
	if err != nil {
		if isFingerprintConflict(err) {
			return fmt.Errorf("create document: %w: %w", types.ErrDuplicateDocument, err)
		}
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
	var modified *time.Time
	err := s.pool.QueryRow(ctx, documentGetSQL, uuid).Scan(
		&doc.UUID, &doc.SourceURI, &doc.Fingerprint, &doc.Title,
		&metaBytes, &doc.CreatedAt, &doc.UpdatedAt, &doc.Scope, &modified)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, types.ErrDocumentNotFound
		}
		return nil, err
	}
	doc.Metadata = decodeMetadata(metaBytes)
	if modified != nil {
		doc.SourceModifiedAt = *modified
	}

	sections, err := s.GetSections(ctx, uuid)
	if err != nil {
		return nil, err
	}
	doc.Sections = sections

	return &doc, nil
}

// FindByFingerprint finds a document by content fingerprint. Fingerprints
// include the document's scope (see types.Fingerprint), so the lookup never
// crosses scopes.
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

// fingerprintIndex is the unique partial index that enforces one document per
// non-empty fingerprint.
const fingerprintIndex = "idx_rag_document_fingerprint"

// isFingerprintConflict reports whether err is a unique violation (SQLSTATE
// 23505) on the fingerprint index, i.e. another document already holds the
// same content fingerprint.
func isFingerprintConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == fingerprintIndex
}

// ListDocumentUUIDs returns the UUID of every stored document in insertion
// order, implementing types.DocumentLister.
func (s *Store) ListDocumentUUIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, documentListSQL)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// FindBySourceURI returns the documents of scope with the given source URI,
// most recently updated first, implementing types.SourceFinder.
func (s *Store) FindBySourceURI(ctx context.Context, scope, sourceURI string) ([]types.SourceDocument, error) {
	rows, err := s.pool.Query(ctx, documentFindSourceSQL, scope, sourceURI)
	if err != nil {
		return nil, fmt.Errorf("find by source URI: %w", err)
	}
	return collectSourceDocuments(rows)
}

// ListSourceDocuments returns the documents of scope whose source URI starts
// with uriPrefix, implementing types.SourceLister.
func (s *Store) ListSourceDocuments(ctx context.Context, scope, uriPrefix string) ([]types.SourceDocument, error) {
	rows, err := s.pool.Query(ctx, documentListSourceSQL, scope, uriPrefix)
	if err != nil {
		return nil, fmt.Errorf("list source documents: %w", err)
	}
	return collectSourceDocuments(rows)
}

func collectSourceDocuments(rows pgx.Rows) ([]types.SourceDocument, error) {
	defer rows.Close()
	out := []types.SourceDocument{}
	for rows.Next() {
		var (
			sd       types.SourceDocument
			modified *time.Time
		)
		if err := rows.Scan(&sd.UUID, &sd.Scope, &sd.SourceURI, &sd.Fingerprint, &modified, &sd.UpdatedAt); err != nil {
			return nil, err
		}
		if modified != nil {
			sd.SourceModifiedAt = *modified
		}
		out = append(out, sd)
	}
	return out, rows.Err()
}

// nullableTime maps the zero time to SQL NULL.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
