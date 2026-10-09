SELECT uuid, scope, source_uri, fingerprint, source_modified_at, COALESCE(updated_at, created_at)
FROM rag_document
WHERE scope = $1 AND starts_with(source_uri, $2)
ORDER BY source_uri, COALESCE(updated_at, created_at) DESC, uuid
