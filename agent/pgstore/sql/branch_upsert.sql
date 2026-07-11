INSERT INTO agent_branch (conversation_id, branch_id, tip_uuid)
VALUES ($1, $2, $3)
ON CONFLICT (conversation_id, branch_id) DO UPDATE SET tip_uuid = EXCLUDED.tip_uuid
