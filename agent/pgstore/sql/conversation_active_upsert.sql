INSERT INTO agent_conversation (conversation_id, active_branch, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (conversation_id) DO UPDATE
  SET active_branch = EXCLUDED.active_branch,
      updated_at    = EXCLUDED.updated_at
