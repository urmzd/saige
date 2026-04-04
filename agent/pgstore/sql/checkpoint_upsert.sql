INSERT INTO agent_checkpoint (uuid, branch_id, node_uuid, name, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (uuid) DO NOTHING
