WITH matched AS (
    SELECT e.id, e.uuid, e.name, e.type, e.summary,
           ts_rank(e.search_vec, plainto_tsquery('english', $1)) AS score
    FROM kg_entity e
    WHERE e.search_vec @@ plainto_tsquery('english', $1)
    ORDER BY score DESC
    LIMIT 20
)
SELECT m.uuid, m.name, m.type, m.summary,
       r.uuid, r.type, r.fact, r.created_at, r.valid_at, r.invalid_at,
       o.uuid, o.name, o.type, o.summary,
       m.score
FROM matched m
JOIN kg_relation r ON (r.source_id = m.id OR r.target_id = m.id) AND r.invalid_at IS NULL
JOIN kg_entity o ON o.id = CASE WHEN r.source_id = m.id THEN r.target_id ELSE r.source_id END
ORDER BY m.score DESC