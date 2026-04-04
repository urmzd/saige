package pgstore

import (
	"context"
	"encoding/json"
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

func childIndex(ctx context.Context, q querier, parentUUID string) (int, error) {
	var idx int
	err := q.QueryRow(ctx,
		`SELECT COALESCE(MAX(child_index), -1) + 1 FROM agent_node WHERE parent_uuid = $1`,
		parentUUID,
	).Scan(&idx)
	return idx, err
}

func saveNode(ctx context.Context, q querier, node *types.Node) error {
	msgBytes, err := tree.MarshalMessage(node.Message)
	if err != nil {
		return fmt.Errorf("marshal node %s: %w", node.ID, err)
	}

	summaryOf := make([]string, len(node.SummaryOf))
	for i, s := range node.SummaryOf {
		summaryOf[i] = string(s)
	}

	cidx, err := childIndex(ctx, q, string(node.ParentID))
	if err != nil {
		return fmt.Errorf("child index for %s: %w", node.ID, err)
	}

	_, err = q.Exec(ctx, nodeUpsertSQL,
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
	)
	if err != nil {
		return fmt.Errorf("upsert node %s: %w", node.ID, err)
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

// SaveNode persists a node with optimistic version checking.
func (s *Store) SaveNode(ctx context.Context, node *types.Node) error {
	return saveNode(ctx, s.pool, node)
}

// LoadNode retrieves a single node by ID.
func (s *Store) LoadNode(ctx context.Context, id types.NodeID) (*types.Node, error) {
	row := s.pool.QueryRow(ctx, nodeGetSQL, string(id))
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
	rows, err := s.pool.Query(ctx, nodeChildrenSQL, string(parentID))
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// LoadPath returns all nodes from root to the given node.
func (s *Store) LoadPath(ctx context.Context, toNodeID types.NodeID) ([]*types.Node, error) {
	rows, err := s.pool.Query(ctx, nodePathSQL, string(toNodeID))
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// LoadTree returns all nodes and branches for a tree rooted at rootID.
func (s *Store) LoadTree(ctx context.Context, rootID types.NodeID) ([]*types.Node, map[types.BranchID]types.NodeID, error) {
	rows, err := s.pool.Query(ctx, nodeTreeSQL, string(rootID))
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
