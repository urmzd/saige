INSERT INTO kg_episode (uuid, name, body, source, group_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id