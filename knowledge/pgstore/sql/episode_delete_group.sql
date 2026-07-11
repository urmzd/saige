DELETE FROM kg_relation WHERE group_id = $1;
DELETE FROM kg_episode WHERE group_id = $1;
DELETE FROM kg_entity WHERE group_id = $1
