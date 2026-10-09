SELECT uuid, parent_uuid, role, message, state, version, depth,
       branch_id, child_index, summary_of, created_at, updated_at,
       archived_at, archived_by
FROM agent_node
WHERE parent_uuid = $1 AND conversation_id = $2
ORDER BY child_index ASC
