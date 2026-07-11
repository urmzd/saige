SELECT uuid, branch_id, node_uuid, name, created_at
FROM agent_checkpoint WHERE conversation_id = $1 AND uuid = $2
