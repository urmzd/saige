SELECT uuid, name, type, summary FROM kg_entity
WHERE group_id = $1 AND name % $2
ORDER BY similarity(name, $2) DESC
LIMIT $3
