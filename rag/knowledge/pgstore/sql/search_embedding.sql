-- Parameters: $1 query embedding, $2 group id (NULL searches every group),
-- $3 result limit (also the per-entity edge cap), $4 as-of time (NULL means
-- currently valid relations).
WITH anchors AS (
    SELECT e.id, 1 - (e.embedding <=> $1::vector) AS score
    FROM kg_entity e
    WHERE e.embedding IS NOT NULL
      AND ($2::text IS NULL OR e.group_id = $2)
    ORDER BY e.embedding <=> $1::vector, e.id
    LIMIT $3
),
anchor_edges AS (
    SELECT edge.id, a.score
    FROM anchors a
    CROSS JOIN LATERAL (
        SELECT r.id
        FROM kg_relation r
        WHERE (r.source_id = a.id OR r.target_id = a.id)
          AND ($2::text IS NULL OR r.group_id = $2)
          AND (($4::timestamptz IS NULL AND r.invalid_at IS NULL)
               OR (r.valid_at <= $4 AND (r.invalid_at IS NULL OR r.invalid_at > $4)))
        ORDER BY r.valid_at DESC, r.id
        LIMIT $3
    ) edge
),
ranked AS (
    SELECT id, max(score) AS score FROM anchor_edges GROUP BY id
)
SELECT src.uuid, src.name, src.type, src.summary,
       r.uuid, r.type, r.fact, r.created_at, r.valid_at, r.invalid_at,
       tgt.uuid, tgt.name, tgt.type, tgt.summary,
       ranked.score
FROM ranked
JOIN kg_relation r ON r.id = ranked.id
JOIN kg_entity src ON src.id = r.source_id
JOIN kg_entity tgt ON tgt.id = r.target_id
ORDER BY ranked.score DESC, r.valid_at DESC, r.uuid
LIMIT $3
