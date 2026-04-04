SELECT uuid, branch_id, node_uuid, name, created_at
FROM agent_checkpoint WHERE uuid = $1
