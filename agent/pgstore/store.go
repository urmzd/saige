// Package pgstore implements agent/types.Store using PostgreSQL.
package pgstore

import (
	"errors"
	"fmt"
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
// ScopedConversationID, or set Config.Scope, so a conversation ID chosen by
// one tenant can never name another tenant's rows.
//
// The pool should already be connected; schema migration is handled
// separately via postgres.RunMigrations.
type Store struct {
	pool           *pgxpool.Pool
	conversationID string
	logger         *slog.Logger
}

// Config names the database and the conversation a Store serves.
type Config struct {
	// Pool is the database. Required. The caller owns it: the store never
	// closes it.
	Pool *pgxpool.Pool
	// ConversationID scopes every node, branch and checkpoint operation, so
	// two conversations can each have a "main" branch without touching each
	// other. An empty ID selects the legacy unscoped namespace (rows written
	// before namespacing existed); use it only for a single-conversation
	// deployment.
	ConversationID string
	// Scope, when set, is a tenant scope: the store's namespace is
	// ScopedConversationID(Scope, ConversationID).
	Scope string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithScope sets Config.Scope.
func WithScope(scope string) Option { return func(c *Config) { c.Scope = scope } }

// WithLogger sets Config.Logger.
func WithLogger(l *slog.Logger) Option { return func(c *Config) { c.Logger = l } }

// New creates a PostgreSQL-backed agent store for cfg's conversation. A nil
// pool or an invalid scope is an error wrapping types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (*Store, error) {
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.Pool == nil {
		return nil, fmt.Errorf("%w: pgstore: Config.Pool is required", types.ErrInvalidConfig)
	}
	id := cfg.ConversationID
	if cfg.Scope != "" {
		scoped, err := ScopedConversationID(cfg.Scope, cfg.ConversationID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", types.ErrInvalidConfig, err)
		}
		id = scoped
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Store{pool: cfg.Pool, conversationID: id, logger: cfg.Logger}, nil
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

// ConversationID returns the namespace this store reads and writes,
// including any tenant scope.
func (s *Store) ConversationID() string { return s.conversationID }
