-- Parameters: $1 edge limit, $2 group id (NULL reads every group).
SELECT src.uuid, src.name, src.type, src.summary,
       r.uuid, r.type, r.fact, r.created_at, r.valid_at, r.invalid_at,
       tgt.uuid, tgt.name, tgt.type, tgt.summary
FROM kg_relation r
JOIN kg_entity src ON src.id = r.source_id
JOIN kg_entity tgt ON tgt.id = r.target_id
WHERE r.invalid_at IS NULL
  AND ($2::text IS NULL OR r.group_id = $2)
ORDER BY r.valid_at DESC, r.id
LIMIT $1
