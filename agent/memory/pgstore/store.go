// Package pgstore implements agent/memory.Store on PostgreSQL 18 with
// hybrid recall.
//
// Each record is embedded with the configured embedder and stored with a
// pgvector embedding and a ParadeDB pg_search BM25 index over its text and
// tags. Recall runs a vector search and a BM25 search inside the caller's
// scope, fuses them with Reciprocal Rank Fusion, adds a small recency term,
// and keeps the best records that fit the token budget. Scope, expiry, kind,
// and time filters are evaluated in SQL, so another tenant's rows never
// reach the process.
//
// The store can also index the turns of past conversations kept by
// agent/pgstore, so an agent can recall what was discussed in earlier
// sessions. Indexing is opt-in, redacts personal data before anything is
// embedded or stored, and is exposed through ConversationTool and the
// Conversations view.
//
// Create the table with postgres.RunMigrations; MigrationOptions sets the
// embedding dimension, which must match the embedder.
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/agent/memory"
	"github.com/urmzd/saige/agent/types"
)

// Defaults for Config fields left at zero.
const (
	DefaultCandidates    = 50
	DefaultFusionK       = 60
	DefaultRecencyWeight = 0.25
	// DefaultRecentLimit bounds how many records an empty-query recall reads
	// before it fits them to the budget.
	DefaultRecentLimit = 200
)

// Origins of stored rows. Memories and indexed conversation turns share one
// table but are never recalled together.
const (
	originMemory       = "memory"
	originConversation = "conversation"
)

var (
	_ memory.Store  = (*Store)(nil)
	_ memory.Lister = (*Store)(nil)
)

// Config configures a Store.
type Config struct {
	// Pool is the database, from postgres.NewPool. Required. The caller
	// owns it.
	Pool *pgxpool.Pool
	// Embedder embeds record text for vector recall. Required. Its vectors
	// must have the dimension of memory_record.embedding (see
	// postgres.MigrationOptions.MemoryEmbeddingDim). Records are embedded
	// with types.PurposeDocument and queries with types.PurposeQuery.
	Embedder types.Embedder
	// Candidates is how many rows the vector search and the BM25 search
	// each return before fusion. 0 uses DefaultCandidates.
	Candidates int
	// MinSimilarity drops vector matches whose cosine similarity to the
	// query is below it, so a query unrelated to every record recalls
	// nothing. The right value depends on the embedding model. 0 keeps
	// every vector candidate.
	MinSimilarity float64
	// FusionK is the Reciprocal Rank Fusion constant. 0 uses
	// DefaultFusionK.
	FusionK int
	// RecencyWeight weights a third ranked list in the fusion: the matched
	// candidates, newest first. It breaks ties toward newer records without
	// recalling anything that did not match. 0 uses DefaultRecencyWeight; a
	// negative value disables it.
	RecencyWeight float64
	// Redact rewrites conversation text before it is embedded or stored by
	// IndexConversation. nil uses privacy.Redact with the default detector,
	// which replaces each detected value with [REDACTED:LABEL].
	Redact func(ctx context.Context, text string) (string, error)
	// ConversationRetention sets ExpiresAt on indexed conversation turns.
	// 0 keeps them until forgotten.
	ConversationRetention time.Duration
	// Now returns the current time. nil uses time.Now.
	Now func() time.Time
}

// Store is a memory.Store backed by PostgreSQL. It is safe for concurrent
// use; the pool is managed by the caller.
type Store struct {
	pool *pgxpool.Pool
	cfg  Config
}

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithRedact sets Config.Redact.
func WithRedact(fn func(ctx context.Context, text string) (string, error)) Option {
	return func(c *Config) { c.Redact = fn }
}

// New returns a store over cfg.Pool. The pool should come from
// postgres.NewPool, which registers the pgvector types. A nil pool or
// embedder is an error wrapping types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (*Store, error) {
	for _, o := range opts {
		o(&cfg)
	}
	pool := cfg.Pool
	if pool == nil {
		return nil, fmt.Errorf("%w: memory pgstore: Config.Pool is required", types.ErrInvalidConfig)
	}
	if cfg.Embedder == nil {
		return nil, fmt.Errorf("%w: memory pgstore: Config.Embedder is required", types.ErrInvalidConfig)
	}
	if cfg.Candidates <= 0 {
		cfg.Candidates = DefaultCandidates
	}
	if cfg.FusionK <= 0 {
		cfg.FusionK = DefaultFusionK
	}
	if cfg.RecencyWeight == 0 {
		cfg.RecencyWeight = DefaultRecencyWeight
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Store{pool: pool, cfg: cfg}, nil
}

func (s *Store) now() time.Time { return s.cfg.Now().UTC() }

// cleanText makes text safe for a TEXT column: valid UTF-8 without NUL.
func cleanText(text string) string {
	if utf8.ValidString(text) && !strings.ContainsRune(text, 0) {
		return text
	}
	return strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\x00", "")
}

// searchText is what BM25 indexes and the embedder sees: the content and
// the tags, since tags are recalled by query too.
func searchText(content string, tags []string) string {
	if len(tags) == 0 {
		return content
	}
	return content + "\n" + strings.Join(tags, " ")
}

func (s *Store) embed(ctx context.Context, purpose types.EmbedPurpose, texts []string) ([][]float32, error) {
	vecs, err := s.cfg.Embedder.Embed(types.WithEmbedPurpose(ctx, purpose), texts)
	if err != nil {
		return nil, fmt.Errorf("pgstore: embed: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("pgstore: embedder returned %d vectors for %d texts", len(vecs), len(texts))
	}
	return vecs, nil
}

// insertSQL stores one row. A row with an idempotency key is never
// replaced, so a replayed write keeps the first content; a row without one
// is replaced, matching the other stores.
const insertSQL = `INSERT INTO memory_record
	(tenant, subject, namespace, id, origin, kind, content, tags, search_text, source, idempotency_key, embedding, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (tenant, subject, namespace, id) DO UPDATE
	SET kind = EXCLUDED.kind, content = EXCLUDED.content, tags = EXCLUDED.tags,
	    search_text = EXCLUDED.search_text, source = EXCLUDED.source,
	    embedding = EXCLUDED.embedding, created_at = EXCLUDED.created_at,
	    expires_at = EXCLUDED.expires_at
	WHERE EXCLUDED.idempotency_key = '' AND memory_record.idempotency_key = ''`

func insertArgs(r memory.Record, origin string, emb []float32) []any {
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	src, _ := json.Marshal(r.Source)
	var expires *time.Time
	if !r.ExpiresAt.IsZero() {
		e := r.ExpiresAt.UTC()
		expires = &e
	}
	return []any{
		r.Scope.Tenant, r.Scope.Subject, r.Scope.Namespace, r.ID, origin, string(r.Kind),
		r.Content, tags, searchText(r.Content, tags), src, r.IdempotencyKey,
		pgvector.NewVector(emb), r.CreatedAt.UTC(), expires,
	}
}

func (s *Store) exists(ctx context.Context, sc memory.Scope, id string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM memory_record WHERE tenant = $1 AND subject = $2 AND namespace = $3 AND id = $4)`,
		sc.Tenant, sc.Subject, sc.Namespace, id).Scan(&ok)
	return ok, err
}

// Remember implements memory.Store. With an IdempotencyKey, a repeat
// returns the first ID without embedding or writing anything, and
// concurrent repeats store one row.
func (s *Store) Remember(ctx context.Context, r memory.Record) (string, error) {
	if err := r.Scope.Validate(); err != nil {
		return "", err
	}
	if r.Scope.ReadOnly {
		return "", memory.ErrReadOnly
	}
	if r.Kind == "" {
		r.Kind = memory.KindSemantic
	}
	if !r.Kind.Valid() {
		return "", fmt.Errorf("memory: unknown kind %q", r.Kind)
	}
	r.Content = cleanText(r.Content)
	if len(r.Tags) > 0 {
		tags := make([]string, len(r.Tags))
		for i, tag := range r.Tags {
			tags[i] = cleanText(tag)
		}
		r.Tags = tags
	}
	if r.ID == "" {
		r.ID = memory.RecordID(r)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.now()
	}
	if r.IdempotencyKey != "" {
		ok, err := s.exists(ctx, r.Scope, r.ID)
		if err != nil {
			return "", fmt.Errorf("pgstore: remember: %w", err)
		}
		if ok {
			return r.ID, nil
		}
	}
	vecs, err := s.embed(ctx, types.PurposeDocument, []string{searchText(r.Content, r.Tags)})
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, insertSQL, insertArgs(r, originMemory, vecs[0])...); err != nil {
		return "", fmt.Errorf("pgstore: remember: %w", err)
	}
	return r.ID, nil
}

// Forget implements memory.Store. Records in sub-namespaces of s can be
// forgotten from s, as Recall from s returns them; a record in s itself is
// preferred when several share the ID.
func (s *Store) Forget(ctx context.Context, sc memory.Scope, id string) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	if sc.ReadOnly {
		return memory.ErrReadOnly
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM memory_record WHERE row_id = (
		SELECT row_id FROM memory_record
		WHERE tenant = $1 AND subject = $2 AND id = $4
		  AND ($3 = '' OR namespace = $3 OR starts_with(namespace, $3 || '/'))
		ORDER BY (namespace = $3) DESC, namespace
		LIMIT 1)`, sc.Tenant, sc.Subject, sc.Namespace, id)
	if err != nil {
		return fmt.Errorf("pgstore: forget: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", memory.ErrNotFound, id)
	}
	return nil
}

// Purge deletes every record, in every scope, that expired before now, and
// returns how many it deleted. Expired records are never recalled, so Purge
// only reclaims space; run it on a schedule to enforce retention at rest.
func (s *Store) Purge(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memory_record WHERE expires_at IS NOT NULL AND expires_at <= $1`, s.now())
	if err != nil {
		return 0, fmt.Errorf("pgstore: purge: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Query is one hybrid recall.
type Query struct {
	// Text is the query. Empty returns the most recent records.
	Text string
	// Budget is the token budget of the result. 0 uses
	// memory.DefaultRecallBudget.
	Budget int
	// Kinds limits recall to these kinds. Empty allows every kind.
	Kinds []memory.Kind
	// Since and Until limit recall to records created in [Since, Until).
	// Zero leaves that side open.
	Since, Until time.Time
	// Conversations searches indexed conversation turns instead of
	// memories.
	Conversations bool
	// Conversation limits a conversation search to one conversation ID.
	Conversation string
}

// filter builds the WHERE clause shared by every recall query, with its
// placeholders numbered after the first `offset` arguments.
func (s *Store) filter(sc memory.Scope, q Query, offset int) (string, []any) {
	args := []any{sc.Tenant, sc.Subject, sc.Namespace, s.now()}
	n := func(i int) int { return offset + i }
	origin := originMemory
	if q.Conversations {
		origin = originConversation
	}
	args = append(args, origin)
	where := fmt.Sprintf(`tenant = $%d AND subject = $%d
	  AND ($%d = '' OR namespace = $%d OR starts_with(namespace, $%d || '/'))
	  AND (expires_at IS NULL OR expires_at > $%d)
	  AND origin = $%d`, n(1), n(2), n(3), n(3), n(3), n(4), n(5))
	if len(q.Kinds) > 0 {
		kinds := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			kinds[i] = string(k)
		}
		args = append(args, kinds)
		where += fmt.Sprintf(" AND kind = ANY($%d)", offset+len(args))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since.UTC())
		where += fmt.Sprintf(" AND created_at >= $%d", offset+len(args))
	}
	if !q.Until.IsZero() {
		args = append(args, q.Until.UTC())
		where += fmt.Sprintf(" AND created_at < $%d", offset+len(args))
	}
	if q.Conversations && q.Conversation != "" {
		args = append(args, q.Conversation)
		where += fmt.Sprintf(" AND source ->> 'conversation' = $%d", offset+len(args))
	}
	return where, args
}

const recordColumns = `tenant, subject, namespace, id, kind, content, tags, source, idempotency_key, created_at, expires_at`

// hit is one recalled row with the score its search gave it.
type hit struct {
	rec   memory.Record
	score float64
}

func (h hit) key() string { return h.rec.Scope.Namespace + "\x00" + h.rec.ID }

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func scanHits(ctx context.Context, q querier, sql string, args []any, scored bool) ([]hit, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hit
	for rows.Next() {
		var (
			h       hit
			kind    string
			src     []byte
			expires *time.Time
		)
		dest := []any{&h.rec.Scope.Tenant, &h.rec.Scope.Subject, &h.rec.Scope.Namespace, &h.rec.ID,
			&kind, &h.rec.Content, &h.rec.Tags, &src, &h.rec.IdempotencyKey, &h.rec.CreatedAt, &expires}
		if scored {
			dest = append(dest, &h.score)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		h.rec.Kind = memory.Kind(kind)
		if len(h.rec.Tags) == 0 {
			h.rec.Tags = nil
		}
		if len(src) > 0 {
			_ = json.Unmarshal(src, &h.rec.Source)
		}
		if expires != nil {
			h.rec.ExpiresAt = *expires
		}
		h.rec.CreatedAt = h.rec.CreatedAt.UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// Recall implements memory.Store with hybrid recall; see Search.
func (s *Store) Recall(ctx context.Context, sc memory.Scope, query string, budget int) ([]memory.Record, error) {
	return s.Search(ctx, sc, Query{Text: query, Budget: budget})
}

// Recent implements memory.Lister.
func (s *Store) Recent(ctx context.Context, sc memory.Scope, limit int) ([]memory.Record, error) {
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	hits, err := s.recent(ctx, sc, Query{}, limit)
	if err != nil {
		return nil, err
	}
	return records(hits), nil
}

func (s *Store) recent(ctx context.Context, sc memory.Scope, q Query, limit int) ([]hit, error) {
	if limit <= 0 {
		limit = DefaultRecentLimit
	}
	where, args := s.filter(sc, q, 0)
	args = append(args, limit)
	sql := fmt.Sprintf(`SELECT %s FROM memory_record WHERE %s ORDER BY created_at DESC, id LIMIT $%d`, recordColumns, where, len(args))
	hits, err := scanHits(ctx, s.pool, sql, args, false)
	if err != nil {
		return nil, fmt.Errorf("pgstore: recent: %w", err)
	}
	return hits, nil
}

func records(hits []hit) []memory.Record {
	out := make([]memory.Record, len(hits))
	for i, h := range hits {
		out[i] = h.rec
	}
	return out
}

// Search recalls the records in sc and its sub-namespaces that best match
// q, most relevant first, within q.Budget tokens.
//
// A query with text runs two searches in SQL, each limited to
// Config.Candidates rows: a pgvector cosine search over the embeddings
// (dropping matches below Config.MinSimilarity) and a pg_search BM25 search
// over the text and tags. Their rankings are fused with Reciprocal Rank
// Fusion, plus a recency ranking of the same candidates weighted by
// Config.RecencyWeight. A record neither search found is never returned.
// An empty query returns the most recent records.
func (s *Store) Search(ctx context.Context, sc memory.Scope, q Query) ([]memory.Record, error) {
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(cleanText(q.Text))
	if text == "" {
		hits, err := s.recent(ctx, sc, q, DefaultRecentLimit)
		if err != nil {
			return nil, err
		}
		return memory.FitBudget(records(hits), q.Budget), nil
	}
	vec, err := s.vectorSearch(ctx, sc, q, text)
	if err != nil {
		return nil, err
	}
	lex, err := s.keywordSearch(ctx, sc, q, text)
	if err != nil {
		return nil, err
	}
	return memory.FitBudget(s.fuse(vec, lex), q.Budget), nil
}

func (s *Store) vectorSearch(ctx context.Context, sc memory.Scope, q Query, text string) ([]hit, error) {
	vecs, err := s.embed(ctx, types.PurposeQuery, []string{text})
	if err != nil {
		return nil, err
	}
	where, args := s.filter(sc, q, 1)
	args = append([]any{pgvector.NewVector(vecs[0])}, args...)
	args = append(args, s.cfg.Candidates)
	sql := fmt.Sprintf(`SELECT %s, 1 - (embedding <=> $1) FROM memory_record
		WHERE embedding IS NOT NULL AND %s
		ORDER BY embedding <=> $1 LIMIT $%d`, recordColumns, where, len(args))
	var hits []hit
	// The scope filter rejects other tenants' nearer vectors, so let the
	// HNSW scan continue until enough rows qualify. set_config(..., true)
	// keeps the setting inside this transaction.
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('hnsw.iterative_scan', 'strict_order', true)`); err != nil {
			return err
		}
		var err error
		hits, err = scanHits(ctx, tx, sql, args, true)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: vector search: %w", err)
	}
	out := hits[:0]
	for _, h := range hits {
		if h.score >= s.cfg.MinSimilarity {
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *Store) keywordSearch(ctx context.Context, sc memory.Scope, q Query, text string) ([]hit, error) {
	where, args := s.filter(sc, q, 1)
	args = append([]any{text}, args...)
	args = append(args, s.cfg.Candidates)
	// The query text is a bind parameter, tokenized by pg_search, never
	// parsed as query syntax.
	sql := fmt.Sprintf(`SELECT %s, pdb.score(row_id)::float8 FROM memory_record
		WHERE row_id @@@ paradedb.match('search_text', $1) AND %s
		ORDER BY pdb.score(row_id) DESC, id LIMIT $%d`, recordColumns, where, len(args))
	hits, err := scanHits(ctx, s.pool, sql, args, true)
	if err != nil {
		return nil, fmt.Errorf("pgstore: keyword search (requires the pg_search extension): %w", err)
	}
	return hits, nil
}

// fuse merges the vector and keyword rankings with Reciprocal Rank Fusion
// and adds the recency ranking of the merged candidates.
func (s *Store) fuse(lists ...[]hit) []memory.Record {
	k := float64(s.cfg.FusionK)
	score := map[string]float64{}
	byKey := map[string]memory.Record{}
	for _, l := range lists {
		for rank, h := range l {
			score[h.key()] += 1 / (k + float64(rank) + 1)
			byKey[h.key()] = h.rec
		}
	}
	cands := make([]memory.Record, 0, len(byKey))
	for _, r := range byKey {
		cands = append(cands, r)
	}
	key := func(r memory.Record) string { return r.Scope.Namespace + "\x00" + r.ID }
	if w := s.cfg.RecencyWeight; w > 0 {
		sort.Slice(cands, func(i, j int) bool {
			if !cands[i].CreatedAt.Equal(cands[j].CreatedAt) {
				return cands[i].CreatedAt.After(cands[j].CreatedAt)
			}
			return key(cands[i]) < key(cands[j])
		})
		for rank, r := range cands {
			score[key(r)] += w / (k + float64(rank) + 1)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := score[key(cands[i])], score[key(cands[j])]
		if a != b {
			return a > b
		}
		if !cands[i].CreatedAt.Equal(cands[j].CreatedAt) {
			return cands[i].CreatedAt.After(cands[j].CreatedAt)
		}
		return key(cands[i]) < key(cands[j])
	})
	return cands
}
