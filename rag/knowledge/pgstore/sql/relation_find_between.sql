SELECT r.uuid, r.type, r.fact, r.created_at, r.valid_at, r.invalid_at,
       src.uuid, tgt.uuid
FROM kg_relation r
JOIN kg_entity src ON src.id = r.source_id
JOIN kg_entity tgt ON tgt.id = r.target_id
WHERE (src.uuid = $1 AND tgt.uuid = $2)
   OR (src.uuid = $2 AND tgt.uuid = $1)