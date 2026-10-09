WITH nodes AS (
    DELETE FROM agent_node WHERE conversation_id = $1
), branches AS (
    DELETE FROM agent_branch WHERE conversation_id = $1
), checkpoints AS (
    DELETE FROM agent_checkpoint WHERE conversation_id = $1
)
DELETE FROM agent_conversation WHERE conversation_id = $1
