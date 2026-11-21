-- Parameters: $1 frontier entity UUIDs, $2 edge limit, $3 relation UUIDs to
-- exclude, $4 group id (NULL traverses every group).
WITH frontier AS (
    SELECT id FROM kg_entity
    WHERE uuid = ANY($1::text[])
      AND ($4::text IS NULL OR group_id = $4)
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
  AND ($4::text IS NULL OR r.group_id = $4)
ORDER BY r.valid_at DESC, r.id
LIMIT $2
