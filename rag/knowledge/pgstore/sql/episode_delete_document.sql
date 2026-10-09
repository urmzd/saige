WITH doc_episodes AS (
    SELECT id FROM kg_episode WHERE group_id = $1 AND document_id = $2
),
doc_entities AS (
    SELECT DISTINCT m.entity_id AS id
    FROM kg_mention m
    WHERE m.episode_id IN (SELECT id FROM doc_episodes)
),
doc_relations AS (
    SELECT DISTINCT re.relation_id AS id
    FROM kg_relation_episode re
    WHERE re.episode_id IN (SELECT id FROM doc_episodes)
),
-- A relation another document also asserts stays.
orphan_relations AS (
    SELECT dr.id
    FROM doc_relations dr
    WHERE NOT EXISTS (
        SELECT 1 FROM kg_relation_episode re
        WHERE re.relation_id = dr.id
          AND re.episode_id NOT IN (SELECT id FROM doc_episodes)
    )
),
deleted_relations AS (
    DELETE FROM kg_relation WHERE id IN (SELECT id FROM orphan_relations)
    RETURNING id
),
-- A surviving relation whose invalid_at equals the valid_at of a deleted
-- relation with the same source, target, and type was ended by it: either
-- superseded by it or backfilled behind it. Its end is recomputed from the
-- surviving relations of that key: the earliest one that would supersede it
-- (valid no earlier, and created later when valid at the same time), or NULL
-- when none is left, so the fact is current again.
restored_relations AS (
    UPDATE kg_relation p
    SET invalid_at = (
        SELECT min(r.valid_at)
        FROM kg_relation r
        WHERE r.source_id = p.source_id
          AND r.target_id = p.target_id
          AND r.type = p.type
          AND r.id <> p.id
          AND r.id NOT IN (SELECT id FROM orphan_relations)
          AND (r.valid_at > p.valid_at OR (r.valid_at = p.valid_at AND r.id > p.id))
    )
    WHERE p.id NOT IN (SELECT id FROM orphan_relations)
      AND EXISTS (
          SELECT 1 FROM kg_relation d
          WHERE d.id IN (SELECT id FROM orphan_relations)
            AND d.source_id = p.source_id
            AND d.target_id = p.target_id
            AND d.type = p.type
            AND p.invalid_at = d.valid_at
      )
    RETURNING p.id
),
deleted_episodes AS (
    DELETE FROM kg_episode WHERE id IN (SELECT id FROM doc_episodes)
    RETURNING id
)
SELECT (SELECT count(*) FROM deleted_episodes), ARRAY(SELECT id FROM doc_entities)
