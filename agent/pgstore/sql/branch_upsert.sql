INSERT INTO agent_branch (branch_id, tip_uuid)
VALUES ($1, $2)
ON CONFLICT (branch_id) DO UPDATE SET tip_uuid = EXCLUDED.tip_uuid
