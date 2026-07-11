INSERT INTO rag_section (uuid, document_id, idx, heading)
VALUES ($1, $2, $3, $4)
RETURNING id
