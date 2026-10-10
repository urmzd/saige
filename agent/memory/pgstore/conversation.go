package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/urmzd/saige/agent/memory"
	agentpg "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// ErrForeignConversation is returned when IndexConversation is asked to
// index a tenant-scoped conversation into another tenant's memory.
var ErrForeignConversation = errors.New("pgstore: conversation belongs to another tenant")

// MaxTurnBytes bounds the text indexed for one conversation turn. Longer
// turns are cut at a character boundary.
const MaxTurnBytes = 4096

// ConversationSource is the Provenance.Source of indexed conversation
// turns.
const ConversationSource = "conversation"

// embedBatch bounds how many turns one Embed call carries.
const embedBatch = 64

// placeholderRE matches a privacy vault placeholder such as <<EMAIL_1>>. A
// placeholder means something only inside the session whose vault issued
// it, so an indexed turn replaces it with a permanent redaction marker.
var placeholderRE = regexp.MustCompile(`<<([A-Z0-9_]+)_[0-9]+>>`)

// turnsSQL reads the user and assistant turns of one conversation from
// agent/pgstore's table, in conversation order. Archived nodes and
// compaction summaries are skipped; the turns a summary replaced are still
// indexed.
const turnsSQL = `SELECT uuid, role, message, created_at FROM agent_node
WHERE conversation_id = $1 AND role IN ('user', 'assistant')
  AND state IN ($2, $3) AND archived_at IS NULL
  AND (summary_of IS NULL OR cardinality(summary_of) = 0)
ORDER BY depth, child_index`

// turnID derives the record ID of one indexed turn, stable across repeated
// indexing.
func turnID(sc memory.Scope, conversationID, nodeID string) string {
	sum := sha256.Sum256([]byte(sc.Key() + "\x00" + conversationID + "\x00" + nodeID))
	return "conv-" + hex.EncodeToString(sum[:10])
}

// IndexConversation indexes the user and assistant turns of a conversation
// stored by agent/pgstore, so they can be recalled in later sessions through
// ConversationTool or the Conversations view. It is opt-in: nothing is
// indexed unless the host calls it, typically after a run ends.
//
// The host chooses both the scope and the conversation; the model never
// names either. A conversation ID built with agentpg.ScopedConversationID
// must belong to sc's tenant, or ErrForeignConversation is returned.
//
// Each turn is redacted with Config.Redact before it is embedded or stored,
// and vault placeholders are replaced with [REDACTED:LABEL]. Injected memory
// blocks and non-text content are skipped. Turns already indexed are not
// embedded again, so calling IndexConversation after every run indexes only
// the new turns. It returns how many turns it added.
func (s *Store) IndexConversation(ctx context.Context, sc memory.Scope, conversationID string) (int, error) {
	if err := sc.Validate(); err != nil {
		return 0, err
	}
	if sc.ReadOnly {
		return 0, memory.ErrReadOnly
	}
	if tenant, _, ok := agentpg.SplitConversationID(conversationID); ok && tenant != sc.Tenant {
		return 0, ErrForeignConversation
	}
	seen, err := s.indexedTurns(ctx, sc, conversationID)
	if err != nil {
		return 0, err
	}
	rows, err := s.pool.Query(ctx, turnsSQL, conversationID, int(types.NodeActive), int(types.NodeCompacted))
	if err != nil {
		return 0, fmt.Errorf("pgstore: read conversation: %w", err)
	}
	type turn struct {
		id, role, text string
		created        time.Time
	}
	var turns []turn
	for rows.Next() {
		var (
			t   turn
			msg json.RawMessage
		)
		if err := rows.Scan(&t.id, &t.role, &msg, &t.created); err != nil {
			rows.Close()
			return 0, err
		}
		if seen[turnID(sc, conversationID, t.id)] {
			continue
		}
		m, err := tree.UnmarshalMessage(types.Role(t.role), msg)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("pgstore: decode turn %s: %w", t.id, err)
		}
		if t.text = messageText(m); t.text != "" {
			turns = append(turns, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	recs := make([]memory.Record, 0, len(turns))
	for _, t := range turns {
		text, err := s.redact(ctx, t.role+": "+t.text)
		if err != nil {
			return 0, err
		}
		r := memory.Record{
			ID:             turnID(sc, conversationID, t.id),
			Scope:          sc,
			Kind:           memory.KindEpisodic,
			Content:        text,
			Source:         memory.Provenance{Conversation: conversationID, Source: ConversationSource},
			CreatedAt:      t.created.UTC(),
			IdempotencyKey: "node:" + t.id,
		}
		if s.cfg.ConversationRetention > 0 {
			r.ExpiresAt = r.CreatedAt.Add(s.cfg.ConversationRetention)
		}
		recs = append(recs, r)
	}

	added := 0
	for start := 0; start < len(recs); start += embedBatch {
		batch := recs[start:min(start+embedBatch, len(recs))]
		texts := make([]string, len(batch))
		for i, r := range batch {
			texts[i] = r.Content
		}
		vecs, err := s.embed(ctx, types.PurposeDocument, texts)
		if err != nil {
			return added, err
		}
		b := &pgx.Batch{}
		for i, r := range batch {
			b.Queue(insertSQL, insertArgs(r, originConversation, vecs[i])...)
		}
		res := s.pool.SendBatch(ctx, b)
		for range batch {
			tag, err := res.Exec()
			if err != nil {
				_ = res.Close()
				return added, fmt.Errorf("pgstore: index conversation: %w", err)
			}
			added += int(tag.RowsAffected())
		}
		if err := res.Close(); err != nil {
			return added, err
		}
	}
	return added, nil
}

func (s *Store) indexedTurns(ctx context.Context, sc memory.Scope, conversationID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM memory_record
		WHERE tenant = $1 AND subject = $2 AND namespace = $3 AND origin = $4 AND source ->> 'conversation' = $5`,
		sc.Tenant, sc.Subject, sc.Namespace, originConversation, conversationID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: read indexed turns: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		seen[id] = true
	}
	return seen, rows.Err()
}

// ForgetConversation deletes every indexed turn of a conversation in sc and
// its sub-namespaces, and returns how many it deleted. The conversation
// itself, in agent/pgstore, is not touched.
func (s *Store) ForgetConversation(ctx context.Context, sc memory.Scope, conversationID string) (int64, error) {
	if err := sc.Validate(); err != nil {
		return 0, err
	}
	if sc.ReadOnly {
		return 0, memory.ErrReadOnly
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM memory_record
		WHERE tenant = $1 AND subject = $2 AND ($3 = '' OR namespace = $3 OR starts_with(namespace, $3 || '/'))
		  AND origin = $4 AND source ->> 'conversation' = $5`,
		sc.Tenant, sc.Subject, sc.Namespace, originConversation, conversationID)
	if err != nil {
		return 0, fmt.Errorf("pgstore: forget conversation: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) redact(ctx context.Context, text string) (string, error) {
	text = placeholderRE.ReplaceAllString(cleanText(text), "[REDACTED:$1]")
	var err error
	if s.cfg.Redact != nil {
		text, err = s.cfg.Redact(ctx, text)
	} else {
		text, err = privacy.Redact(ctx, nil, text)
	}
	if err != nil {
		return "", fmt.Errorf("pgstore: redact conversation: %w", err)
	}
	return text, nil
}

// messageText returns the text of a user or assistant message, without
// tool calls, files, or injected memory blocks, cut to MaxTurnBytes.
func messageText(m types.Message) string {
	var parts []string
	add := func(c any) {
		if t, ok := c.(types.TextPart); ok && strings.TrimSpace(t.Text) != "" && !memory.IsInjected(t.Text) {
			parts = append(parts, strings.TrimSpace(t.Text))
		}
	}
	switch m := m.(type) {
	case types.UserMessage:
		for _, c := range m.Parts {
			add(c)
		}
	case types.AssistantMessage:
		for _, c := range m.Parts {
			add(c)
		}
	}
	text := strings.Join(parts, "\n")
	if len(text) > MaxTurnBytes {
		cut := MaxTurnBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text
}

// Conversations returns a read-only memory.Store view over indexed
// conversation turns. Its Recall searches turns as Search does with
// Query.Conversations; Remember returns memory.ErrUnsupported (turns are
// added by IndexConversation); Forget deletes one indexed turn.
//
// Pass it to memory.Policy.StartMessage or memory.InjectMessage to inject
// past turns at the start of a run.
func (s *Store) Conversations() memory.Store { return conversationView{s} }

type conversationView struct{ s *Store }

func (v conversationView) Remember(context.Context, memory.Record) (string, error) {
	return "", fmt.Errorf("%w: conversation turns are added by IndexConversation", memory.ErrUnsupported)
}

func (v conversationView) Recall(ctx context.Context, sc memory.Scope, query string, budget int) ([]memory.Record, error) {
	return v.s.Search(ctx, sc, Query{Text: query, Budget: budget, Conversations: true})
}

func (v conversationView) Forget(ctx context.Context, sc memory.Scope, id string) error {
	return v.s.Forget(ctx, sc, id)
}

func (v conversationView) Recent(ctx context.Context, sc memory.Scope, limit int) ([]memory.Record, error) {
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	hits, err := v.s.recent(ctx, sc, Query{Conversations: true}, limit)
	if err != nil {
		return nil, err
	}
	return records(hits), nil
}
