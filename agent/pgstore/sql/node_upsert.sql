INSERT INTO agent_node (uuid, parent_uuid, role, message, state, version, depth,
                        branch_id, child_index, summary_of, created_at, updated_at,
                        archived_at, archived_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (uuid) DO UPDATE
  SET message     = EXCLUDED.message,
      state       = EXCLUDED.state,
      version     = EXCLUDED.version,
      updated_at  = EXCLUDED.updated_at,
      archived_at = EXCLUDED.archived_at,
      archived_by = EXCLUDED.archived_by,
      summary_of  = EXCLUDED.summary_of
  WHERE agent_node.version < EXCLUDED.version
