package online

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/types"
)

// LabelScope is the record label PGSource sets to its tenant scope.
const LabelScope = "scope"

// PGSource reads finished runs from conversations stored by agent/pgstore.
// A run ends at an active assistant node with no tool calls; the window
// selects on that node's creation time, through the partial index
// postgres.RunMigrations creates for it.
type PGSource struct {
	Pool *pgxpool.Pool
	// Scope limits the source to one tenant's conversations, those written
	// through pgstore.NewScopedStore with this scope. Records then name
	// conversations without the scope prefix and carry LabelScope. Empty
	// reads every conversation.
	Scope string
	// Limit caps the records one Records call returns, oldest first; zero
	// means no limit.
	Limit int
}

// Records implements [Source].
func (s PGSource) Records(ctx context.Context, w Window) ([]Record, error) {
	prefix := ""
	if s.Scope != "" {
		p, err := pgstore.ScopedConversationID(s.Scope, "")
		if err != nil {
			return nil, err
		}
		prefix = p
	}
	var to *time.Time
	if !w.To.IsZero() {
		to = &w.To
	}
	var limit *int
	if s.Limit > 0 {
		limit = &s.Limit
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT conversation_id, uuid FROM agent_node
		WHERE role = 'assistant' AND state = $1
		  AND created_at >= $2 AND ($3::timestamptz IS NULL OR created_at < $3)
		  AND NOT (message->'content' @> '[{"type": "tool_use"}]'::jsonb)
		  AND ($4 = '' OR starts_with(conversation_id, $4))
		ORDER BY created_at, uuid
		LIMIT $5`, int(types.NodeActive), w.From, to, prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("online: find finished runs: %w", err)
	}
	type candidate struct{ conversation, node string }
	cands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.conversation, &c.node)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("online: find finished runs: %w", err)
	}
	out := make([]Record, 0, len(cands))
	for _, c := range cands {
		rec, err := s.load(ctx, c.conversation, c.node)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	sortRecords(out)
	return out, nil
}

// Lookup implements [Source]. ref.Conversation is unscoped when the source
// has a Scope.
func (s PGSource) Lookup(ctx context.Context, ref Ref) (Record, error) {
	conv := ref.Conversation
	if s.Scope != "" {
		scoped, err := pgstore.ScopedConversationID(s.Scope, conv)
		if err != nil {
			return Record{}, err
		}
		conv = scoped
	}
	rec, err := s.load(ctx, conv, ref.Node)
	if err != nil {
		return Record{}, err
	}
	rec.Ref.TraceID, rec.Ref.SpanID = ref.TraceID, ref.SpanID
	return rec, nil
}

func (s PGSource) load(ctx context.Context, conversation, node string) (Record, error) {
	path, err := pgstore.NewStore(s.Pool, conversation, nil).LoadPath(ctx, types.NodeID(node))
	if err != nil {
		return Record{}, fmt.Errorf("online: load %s: %w", node, err)
	}
	// A missing node, or one stored under another conversation, loads as
	// an empty path, which FromPath reports as ErrNotFinished.
	name := conversation
	if s.Scope != "" {
		_, name, _ = pgstore.SplitConversationID(conversation)
	}
	rec, err := FromPath(name, path)
	if err != nil {
		return Record{}, err
	}
	if s.Scope != "" {
		rec.Labels = map[string]string{LabelScope: s.Scope}
	}
	return rec, nil
}
