INSERT INTO kg_mention (episode_id, entity_id)
SELECT ep.id, e.id
FROM kg_episode ep
JOIN kg_entity e ON e.uuid = ANY($2::text[])
WHERE ep.uuid = $1
ON CONFLICT DO NOTHING
