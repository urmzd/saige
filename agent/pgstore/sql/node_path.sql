WITH RECURSIVE ancestors AS (
    SELECT uuid, parent_uuid, role, message, state, version, depth,
           branch_id, child_index, summary_of, created_at, updated_at,
           archived_at, archived_by
    FROM agent_node WHERE uuid = $1
    UNION ALL
    SELECT n.uuid, n.parent_uuid, n.role, n.message, n.state, n.version,
           n.depth, n.branch_id, n.child_index, n.summary_of,
           n.created_at, n.updated_at, n.archived_at, n.archived_by
    FROM agent_node n
    JOIN ancestors a ON n.uuid = a.parent_uuid
    WHERE a.parent_uuid != ''
)
SELECT * FROM ancestors ORDER BY depth ASC
