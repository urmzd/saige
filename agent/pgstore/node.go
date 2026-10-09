package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// querier abstracts pgxpool.Pool and pgx.Tx so node helpers work in both contexts.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ErrConversationMismatch reports a node write for a UUID that another
// conversation already owns.
var ErrConversationMismatch = errors.New("pgstore: node belongs to another conversation")

// childIndex returns the next sibling position under parentUUID. It takes a
// transaction-scoped advisory lock on the parent first, so concurrent writers
// adding the first children of one parent get distinct positions. q must be a
// transaction.
func childIndex(ctx context.Context, q querier, conversationID, parentUUID string) (int, error) {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"saige.agent_node.children:"+parentUUID); err != nil {
		return 0, err
	}
	var idx int
	err := q.QueryRow(ctx,
		`SELECT COALESCE(MAX(child_index), -1) + 1 FROM agent_node WHERE parent_uuid = $1 AND conversation_id = $2`,
		parentUUID, conversationID,
	).Scan(&idx)
	return idx, err
}

// saveNode upserts node within conversationID. A write at a version lower than
// the stored one returns tree.ErrVersionConflict; a write at the stored
// version is a no-op. q must be a transaction.
func saveNode(ctx context.Context, q querier, conversationID string, node *types.Node) error {
	msgBytes, err := tree.MarshalMessage(node.Message)
	if err != nil {
		return fmt.Errorf("marshal node %s: %w", node.ID, err)
	}

	summaryOf := make([]string, len(node.SummaryOf))
	for i, s := range node.SummaryOf {
		summaryOf[i] = string(s)
	}

	cidx, err := childIndex(ctx, q, conversationID, string(node.ParentID))
	if err != nil {
		return fmt.Errorf("child index for %s: %w", node.ID, err)
	}

	tag, err := q.Exec(ctx, nodeUpsertSQL,
		string(node.ID),
		string(node.ParentID),
		string(node.Message.Role()),
		msgBytes,
		int(node.State),
		int64(node.Version), //nolint:gosec // version values never exceed int64 range
		node.Depth,
		string(node.BranchID),
		cidx,
		summaryOf,
		node.CreatedAt,
		node.UpdatedAt,
		node.ArchivedAt,
		node.ArchivedBy,
		conversationID,
	)
	if err != nil {
		return fmt.Errorf("upsert node %s: %w", node.ID, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	// The upsert skipped the row: find out whether it was a same-version
	// rewrite (fine), a stale write, or another conversation's node.
	var (
		owner  string
		stored int64
	)
	if err := q.QueryRow(ctx, nodeVersionSQL, string(node.ID)).Scan(&owner, &stored); err != nil {
		return fmt.Errorf("check node %s version: %w", node.ID, err)
	}
	switch {
	case owner != conversationID:
		return fmt.Errorf("%w: node %s", ErrConversationMismatch, node.ID)
	case uint64(stored) > node.Version: //nolint:gosec // version stored as int64 in DB, always non-negative
		return fmt.Errorf("%w: node %s version %d is older than stored version %d",
			tree.ErrVersionConflict, node.ID, node.Version, stored)
	}
	return nil
}

func scanNode(rows pgx.Row) (*types.Node, error) {
	var (
		id         string
		parentUUID string
		role       string
		message    json.RawMessage
		state      int
		version    int64
		depth      int
		branchID   string
		childIndex int
		summaryOf  []string
		createdAt  time.Time
		updatedAt  time.Time
		archivedAt *time.Time
		archivedBy string
	)

	err := rows.Scan(&id, &parentUUID, &role, &message, &state, &version, &depth,
		&branchID, &childIndex, &summaryOf, &createdAt, &updatedAt,
		&archivedAt, &archivedBy)
	if err != nil {
		return nil, err
	}

	msg, err := tree.UnmarshalMessage(types.Role(role), message)
	if err != nil {
		return nil, fmt.Errorf("unmarshal node %s: %w", id, err)
	}

	nids := make([]types.NodeID, len(summaryOf))
	for i, s := range summaryOf {
		nids[i] = types.NodeID(s)
	}

	return &types.Node{
		ID:         types.NodeID(id),
		ParentID:   types.NodeID(parentUUID),
		Message:    msg,
		State:      types.NodeState(state),
		Version:    uint64(version), //nolint:gosec // version stored as int64 in DB, always non-negative
		Depth:      depth,
		BranchID:   types.BranchID(branchID),
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
		ArchivedAt: archivedAt,
		ArchivedBy: archivedBy,
		SummaryOf:  nids,
	}, nil
}

func scanNodes(rows pgx.Rows) ([]*types.Node, error) {
	defer rows.Close()
	var nodes []*types.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// SaveNode persists a node within this store's conversation. A stale Version
// returns tree.ErrVersionConflict, the stored Version is a no-op, and a UUID
// owned by another conversation returns ErrConversationMismatch.
func (s *Store) SaveNode(ctx context.Context, node *types.Node) error {
	return s.Tx(ctx, func(tx types.StoreTx) error { return tx.SaveNode(ctx, node) })
}

// LoadNode retrieves a single node by ID within this store's conversation.
func (s *Store) LoadNode(ctx context.Context, id types.NodeID) (*types.Node, error) {
	row := s.pool.QueryRow(ctx, nodeGetSQL, string(id), s.conversationID)
	n, err := scanNode(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("node not found: %s", id)
		}
		return nil, err
	}
	return n, nil
}

// LoadChildren returns direct children of a node, ordered by child_index.
func (s *Store) LoadChildren(ctx context.Context, parentID types.NodeID) ([]*types.Node, error) {
	rows, err := s.pool.Query(ctx, nodeChildrenSQL, string(parentID), s.conversationID)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// LoadPath returns all nodes from root to the given node.
func (s *Store) LoadPath(ctx context.Context, toNodeID types.NodeID) ([]*types.Node, error) {
	rows, err := s.pool.Query(ctx, nodePathSQL, string(toNodeID), s.conversationID)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// LoadTree returns all nodes and branches for a tree rooted at rootID.
func (s *Store) LoadTree(ctx context.Context, rootID types.NodeID) ([]*types.Node, map[types.BranchID]types.NodeID, error) {
	rows, err := s.pool.Query(ctx, nodeTreeSQL, string(rootID), s.conversationID)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, nil, err
	}

	branches, err := s.ListBranches(ctx)
	if err != nil {
		return nil, nil, err
	}

	return nodes, branches, nil
}
