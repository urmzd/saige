SELECT DISTINCT ep.uuid, ep.name, ep.body, ep.source, ep.group_id, ep.metadata, ep.created_at
FROM kg_episode ep
JOIN kg_mention m ON m.episode_id = ep.id
JOIN kg_relation r ON (r.source_id = m.entity_id OR r.target_id = m.entity_id)
WHERE r.uuid = $1