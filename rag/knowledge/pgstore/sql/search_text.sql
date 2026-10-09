-- Parameters: $1 query text, $2 group id (NULL searches every group),
-- $3 result limit (also the per-entity edge cap), $4 as-of time (NULL means
-- currently valid relations).
WITH q AS (
    SELECT plainto_tsquery('english', $1) AS query
),
anchors AS (
    SELECT e.id, ts_rank(e.search_vec, q.query) AS score
    FROM kg_entity e, q
    WHERE e.search_vec @@ q.query
      AND ($2::text IS NULL OR e.group_id = $2)
    ORDER BY score DESC, e.id
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
-- Facts whose own text or type matches, even when neither entity does.
fact_edges AS (
    SELECT r.id,
           ts_rank(to_tsvector('english', coalesce(r.fact, '') || ' ' || replace(coalesce(r.type, ''), '_', ' ')), q.query) AS score
    FROM kg_relation r, q
    WHERE to_tsvector('english', coalesce(r.fact, '') || ' ' || replace(coalesce(r.type, ''), '_', ' ')) @@ q.query
      AND ($2::text IS NULL OR r.group_id = $2)
      AND (($4::timestamptz IS NULL AND r.invalid_at IS NULL)
           OR (r.valid_at <= $4 AND (r.invalid_at IS NULL OR r.invalid_at > $4)))
    ORDER BY score DESC, r.id
    LIMIT $3
),
ranked AS (
    SELECT id, max(score) AS score
    FROM (SELECT id, score FROM anchor_edges UNION ALL SELECT id, score FROM fact_edges) u
    GROUP BY id
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
