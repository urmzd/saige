package pgstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// MessageMigration reports a MigrateMessages pass.
type MessageMigration struct {
	// Outdated counts node messages stored in an older format.
	Outdated int
	// Rewritten counts the ones written in the current format. It is zero
	// for a dry run, and lower than Outdated when a row changed during the
	// pass (its new contents were kept).
	Rewritten int
}

// migrateBatch bounds the rows one migration query reads.
const migrateBatch = 500

// MigrateMessages rewrites the node messages of this store's conversation
// that an older release stored, in the current format
// (tree.MessageFormatVersion). See MigrateAllMessages.
func (s *Store) MigrateMessages(ctx context.Context, dryRun bool) (MessageMigration, error) {
	return migrateMessages(ctx, s.pool, &s.conversationID, dryRun)
}

// MigrateAllMessages rewrites every node message an older release stored,
// in every conversation, in the current format (tree.MessageFormatVersion).
//
// It is optional: reads convert older messages on the fly, and nothing
// writes the older format again. Rewriting saves that conversion on later
// reads. It changes only the message column: node versions and timestamps
// stay as they were. A row that changes during the pass keeps its new
// contents. There is no way back to the older format, so take a snapshot
// of the database first. With dryRun, outdated rows are counted and checked
// for readability, and nothing is written.
func MigrateAllMessages(ctx context.Context, pool *pgxpool.Pool, dryRun bool) (MessageMigration, error) {
	return migrateMessages(ctx, pool, nil, dryRun)
}

func migrateMessages(ctx context.Context, pool *pgxpool.Pool, conversationID *string, dryRun bool) (MessageMigration, error) {
	var res MessageMigration
	version := fmt.Sprint(tree.MessageFormatVersion)
	after := int64(0)
	for {
		rows, err := pool.Query(ctx, `SELECT id, uuid, role, message FROM agent_node
			WHERE id > $1 AND ($2::text IS NULL OR conversation_id = $2)
			  AND (jsonb_typeof(message) <> 'object' OR message->>'v' IS DISTINCT FROM $3)
			ORDER BY id LIMIT $4`, after, conversationID, version, migrateBatch)
		if err != nil {
			return res, fmt.Errorf("pgstore: read node messages: %w", err)
		}
		type row struct {
			id      int64
			uuid    string
			role    string
			message json.RawMessage
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.uuid, &r.role, &r.message); err != nil {
				rows.Close()
				return res, err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		for _, r := range batch {
			after = r.id
			out, changed, err := tree.MigrateMessage(types.Role(r.role), r.message)
			if err != nil {
				return res, fmt.Errorf("pgstore: node %s: %w", r.uuid, err)
			}
			if !changed {
				continue
			}
			res.Outdated++
			if dryRun {
				continue
			}
			tag, err := pool.Exec(ctx, `UPDATE agent_node SET message = $1 WHERE id = $2 AND message = $3::jsonb`,
				out, r.id, r.message)
			if err != nil {
				return res, fmt.Errorf("pgstore: rewrite node %s: %w", r.uuid, err)
			}
			res.Rewritten += int(tag.RowsAffected())
		}
		if len(batch) < migrateBatch {
			return res, nil
		}
	}
}
