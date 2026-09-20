SELECT uuid, name, type, summary FROM kg_entity
WHERE name % $1
ORDER BY similarity(name, $1) DESC
LIMIT $2