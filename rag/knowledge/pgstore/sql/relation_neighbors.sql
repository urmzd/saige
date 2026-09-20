SELECT r.uuid, r.type, r.fact, r.created_at, r.valid_at, r.invalid_at,
       CASE WHEN r.source_id = $1 THEN tgt.uuid ELSE src.uuid END,
       CASE WHEN r.source_id = $1 THEN tgt.name ELSE src.name END,
       CASE WHEN r.source_id = $1 THEN tgt.type ELSE src.type END,
       CASE WHEN r.source_id = $1 THEN tgt.summary ELSE src.summary END,
       r.source_id = $1 AS is_outgoing
FROM kg_relation r
JOIN kg_entity src ON src.id = r.source_id
JOIN kg_entity tgt ON tgt.id = r.target_id
WHERE (r.source_id = $1 OR r.target_id = $1)
  AND r.invalid_at IS NULL