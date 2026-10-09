INSERT INTO kg_relation_episode (relation_id, episode_id)
SELECT r.id, ep.id
FROM kg_relation r, kg_episode ep
WHERE r.uuid = $1 AND ep.uuid = $2
ON CONFLICT DO NOTHING
