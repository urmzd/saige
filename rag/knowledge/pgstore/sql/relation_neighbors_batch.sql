WITH frontier AS (
    SELECT id FROM kg_entity WHERE uuid = ANY($1::text[])
)
SELECT r.uuid, r.type, r.fact, r.created_at, r.valid_at, r.invalid_at,
       src.uuid, src.name, src.type, src.summary,
       tgt.uuid, tgt.name, tgt.type, tgt.summary
FROM kg_relation r
JOIN kg_entity src ON src.id = r.source_id
JOIN kg_entity tgt ON tgt.id = r.target_id
WHERE (r.source_id IN (SELECT id FROM frontier) OR r.target_id IN (SELECT id FROM frontier))
  AND r.invalid_at IS NULL
  AND r.uuid <> ALL($3::text[])
ORDER BY r.valid_at DESC, r.id
LIMIT $2
