INSERT INTO kg_episode (uuid, name, body, source, group_id, document_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id
