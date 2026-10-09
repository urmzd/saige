package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/agent/types"
)

func saveActiveBranch(ctx context.Context, q querier, conversationID string, branch types.BranchID) error {
	if _, err := q.Exec(ctx, conversationActiveUpsertSQL, conversationID, string(branch)); err != nil {
		return fmt.Errorf("save active branch %s: %w", branch, err)
	}
	return nil
}

// SaveActiveBranch records the branch a reloaded tree should make active.
func (s *Store) SaveActiveBranch(ctx context.Context, branch types.BranchID) error {
	return saveActiveBranch(ctx, s.pool, s.conversationID, branch)
}

// LoadActiveBranch returns the saved active branch, or "" when none was saved.
func (s *Store) LoadActiveBranch(ctx context.Context) (types.BranchID, error) {
	var branch string
	err := s.pool.QueryRow(ctx, conversationActiveGetSQL, s.conversationID).Scan(&branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return types.BranchID(branch), nil
}

// DeleteConversation removes every node, branch, checkpoint and the saved
// active branch of this store's conversation in one transaction. Other
// conversations are untouched. On the legacy "" namespace it removes rows
// that were never assigned to a conversation.
func (s *Store) DeleteConversation(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, conversationDeleteSQL, s.conversationID); err != nil {
		return fmt.Errorf("delete conversation %q: %w", s.conversationID, err)
	}
	return nil
}
