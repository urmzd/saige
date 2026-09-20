INSERT INTO kg_entity (uuid, name, type, summary, embedding, group_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (group_id, name, type) DO UPDATE
  SET summary = EXCLUDED.summary,
      embedding = COALESCE(EXCLUDED.embedding, kg_entity.embedding)
RETURNING uuid