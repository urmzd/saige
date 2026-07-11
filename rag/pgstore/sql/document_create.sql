INSERT INTO rag_document (uuid, source_uri, fingerprint, title, metadata, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id
