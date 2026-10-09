// Package pgstore implements agent/types.Store using PostgreSQL.
package pgstore

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

var (
	_ types.Store             = (*Store)(nil)
	_ tree.ActiveBranchWriter = (*Store)(nil)
	_ tree.ActiveBranchReader = (*Store)(nil)
	_ tree.ActiveBranchWriter = (*pgStoreTx)(nil)
)

// Store implements types.Store backed by PostgreSQL.
//
// A Store is scoped to a single conversation: node, branch, checkpoint and
// active-branch rows are namespaced by conversationID, so a Store never reads
// or overwrites another conversation's nodes, and every conversation gets its
// own branch map (each tree starts with a "main" branch, and without namespacing two
// persisted conversations would overwrite each other's tips). Create one Store
// per conversation, mirroring how one memstore instance backs one tree. The
// root node ID of the tree is a natural choice of conversation ID.
//
// For a multi-tenant host, build the conversation ID with
// ScopedConversationID, or use NewScopedStore, so a conversation ID chosen by
// one tenant can never name another tenant's rows.
//
// The pool should already be connected; schema migration is handled
// separately via postgres.RunMigrations.
type Store struct {
	pool           *pgxpool.Pool
	conversationID string
	logger         *slog.Logger
}

// NewStore creates a PostgreSQL-backed agent store scoped to conversationID.
// All node, branch and checkpoint operations are isolated to that namespace, so two
// conversations can each have a "main" branch without touching each other.
// An empty conversationID selects the legacy unscoped namespace (rows written
// before namespacing existed) and should only be used by single-conversation
// deployments.
func NewStore(pool *pgxpool.Pool, conversationID string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{pool: pool, conversationID: conversationID, logger: logger}
}

// scopeSeparator joins a tenant scope and a conversation ID. It is an ASCII
// control character, so it never appears in an ordinary identifier.
const scopeSeparator = "\x1f"

// ScopedConversationID returns the conversation ID that isolates
// conversationID inside a tenant scope. Two tenants that use the same
// conversation ID get different namespaces, and no unscoped ID collides with
// a scoped one unless it contains the unit separator (0x1f). scope must be
// non-empty and must not contain the separator.
func ScopedConversationID(scope, conversationID string) (string, error) {
	if scope == "" {
		return "", errors.New("pgstore: empty tenant scope")
	}
	if strings.Contains(scope, scopeSeparator) {
		return "", errors.New("pgstore: tenant scope contains the 0x1f separator")
	}
	return scopeSeparator + scope + scopeSeparator + conversationID, nil
}

// SplitConversationID reverses ScopedConversationID. ok is false for an
// unscoped conversation ID.
func SplitConversationID(id string) (scope, conversationID string, ok bool) {
	rest, found := strings.CutPrefix(id, scopeSeparator)
	if !found {
		return "", id, false
	}
	scope, conversationID, ok = strings.Cut(rest, scopeSeparator)
	if !ok || scope == "" {
		return "", id, false
	}
	return scope, conversationID, true
}

// NewScopedStore creates a Store for conversationID inside a tenant scope.
// It is NewStore with the ID from ScopedConversationID.
func NewScopedStore(pool *pgxpool.Pool, scope, conversationID string, logger *slog.Logger) (*Store, error) {
	id, err := ScopedConversationID(scope, conversationID)
	if err != nil {
		return nil, err
	}
	return NewStore(pool, id, logger), nil
}

// ConversationID returns the namespace this store reads and writes,
// including any tenant scope.
func (s *Store) ConversationID() string { return s.conversationID }
