INSERT INTO rag_document (uuid, source_uri, fingerprint, title, metadata, created_at, updated_at, scope, source_modified_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id
