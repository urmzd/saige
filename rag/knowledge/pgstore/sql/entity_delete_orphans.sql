DELETE FROM kg_entity e
WHERE e.id = ANY($1::bigint[])
  AND NOT EXISTS (SELECT 1 FROM kg_mention m WHERE m.entity_id = e.id)
  AND NOT EXISTS (SELECT 1 FROM kg_relation r WHERE r.source_id = e.id OR r.target_id = e.id)
