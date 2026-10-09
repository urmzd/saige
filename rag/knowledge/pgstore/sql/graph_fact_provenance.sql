WITH rel AS (
    SELECT id, source_id, target_id FROM kg_relation WHERE uuid = $1
),
linked AS (
    SELECT re.episode_id AS id
    FROM kg_relation_episode re
    JOIN rel ON rel.id = re.relation_id
),
-- Relations written before episode links existed fall back to the episodes
-- that mention both endpoints.
mentioned AS (
    SELECT ms.episode_id AS id
    FROM rel
    JOIN kg_mention ms ON ms.entity_id = rel.source_id
    JOIN kg_mention mt ON mt.episode_id = ms.episode_id AND mt.entity_id = rel.target_id
    WHERE NOT EXISTS (SELECT 1 FROM linked)
)
SELECT ep.uuid, ep.name, ep.body, ep.source, ep.group_id, ep.document_id, ep.metadata, ep.created_at
FROM kg_episode ep
WHERE ep.id IN (SELECT id FROM linked UNION SELECT id FROM mentioned)
ORDER BY ep.created_at, ep.id
