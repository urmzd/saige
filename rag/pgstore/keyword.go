package pgstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/urmzd/saige/rag/internal/textclean"
	"github.com/urmzd/saige/rag/types"
)

var (
	_ types.KeywordSearcher = (*Store)(nil)
	_ types.Retriever       = (*KeywordRetriever)(nil)
	_ types.Named           = (*KeywordRetriever)(nil)
)

// keywordScoreSQL is the BM25 score pg_search assigns to a variant matched
// by the @@@ condition of a keyword query. It is a real; the projection casts
// it to float8, but WHERE and ORDER BY use it uncast because pg_search
// 0.26 rejects a cast score in a WHERE clause.
const keywordScoreSQL = `pdb.score(v.id)`

// Fields of the BM25 index on rag_variant (idx_rag_variant_bm25). The
// section heading and document title are copied onto each variant when it
// is inserted, because a BM25 index covers the columns of one table.
const (
	fieldBody    = "text"
	fieldHeading = "section_heading"
	fieldTitle   = "document_title"
)

// termsSQL tokenizes a text placeholder with pdb.unicode_words, the
// tokenizer the index uses for every field, so prefix and phrase queries
// see the same terms as the index.
const termsSQL = `%s::text::pdb.unicode_words::text[]`

// keywordBuilder compiles a types.KeywordQuery into a pg_search query
// expression. Every value that comes from the query, text, distances,
// slops, boosts, and highlight tags, is a bind parameter; the only text the
// builder writes into SQL is fixed: function names, field names, and
// literal flags it chooses itself.
type keywordBuilder struct {
	args   []any
	boosts map[string]string // field -> boost placeholder
}

// param appends v to the arguments and returns its placeholder.
func (b *keywordBuilder) param(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// buildKeywordSQL builds the full keyword-search query and its argument
// list for a validated, non-empty q. The scope is always $1; the query's
// values follow. Scope, time range, content types, and metadata filters are
// the same clauses vector search uses (see buildSearchSQL); MinScore
// compares against the BM25 score. Ties are broken by variant UUID so
// results are deterministic.
func buildKeywordSQL(q types.KeywordQuery, opts *types.SearchOptions, limit int) (string, []any) {
	b := &keywordBuilder{args: []any{scopeOf(opts)}}
	match := b.query(q)

	extra := ""
	if q.Highlight != nil {
		h := q.Highlight.WithDefaults()
		extra = fmt.Sprintf(`,
	                 pdb.snippet(v.text, start_tag => %s::text, end_tag => %s::text, max_num_chars => %s::int),
	                 pdb.snippet_positions(v.text)`,
			b.param(textclean.String(h.StartTag)), b.param(textclean.String(h.EndTag)), b.param(h.MaxChars))
	}
	base := fmt.Sprintf(searchProjectionSQL, keywordScoreSQL+`::float8`, extra, `v.id @@@ `+match, "$1")
	return buildScopedSQL(base, keywordScoreSQL,
		" ORDER BY "+keywordScoreSQL+" DESC, v.uuid", b.args, opts, limit)
}

// query compiles the whole query. The main clause and Must are required,
// Should only adds to the score unless nothing is required, and MustNot
// excludes. A lone required clause compiles to that clause alone.
func (b *keywordBuilder) query(q types.KeywordQuery) string {
	boosts := q.Boosts()
	var must, should, mustNot []string
	if main := q.Main(); !blank(main.Text) {
		must = append(must, b.clause(main, boosts))
	}
	for _, c := range q.Must {
		if !blank(c.Text) {
			must = append(must, b.clause(c, boosts))
		}
	}
	for _, c := range q.Should {
		if !blank(c.Text) {
			should = append(should, b.clause(c, boosts))
		}
	}
	for _, c := range q.MustNot {
		if !blank(c.Text) {
			mustNot = append(mustNot, b.clause(c, boosts))
		}
	}
	if len(must) == 1 && len(should) == 0 && len(mustNot) == 0 {
		return must[0]
	}
	var parts []string
	for _, group := range []struct {
		name    string
		clauses []string
	}{{"must", must}, {"should", should}, {"must_not", mustNot}} {
		if len(group.clauses) > 0 {
			parts = append(parts, group.name+" => ARRAY["+strings.Join(group.clauses, ", ")+"]")
		}
	}
	return "paradedb.boolean(" + strings.Join(parts, ", ") + ")"
}

// clause compiles one clause over every field with a positive boost. A
// match in any field counts, weighted by the field's boost.
func (b *keywordBuilder) clause(c types.KeywordClause, boosts types.FieldBoosts) string {
	text := b.param(textclean.String(c.Text))
	var parts []string
	for _, f := range []struct {
		name  string
		boost float64
	}{{fieldBody, boosts.Body}, {fieldTitle, boosts.Title}, {fieldHeading, boosts.Heading}} {
		if f.boost <= 0 {
			continue
		}
		q := b.fieldClause(f.name, c, text)
		if f.boost != 1 {
			q = fmt.Sprintf("paradedb.boost(%s::real, %s)", b.boost(f.name, f.boost), q)
		}
		parts = append(parts, q)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "paradedb.boolean(should => ARRAY[" + strings.Join(parts, ", ") + "])"
}

// boost returns the placeholder of a field's boost, binding it once per
// query.
func (b *keywordBuilder) boost(field string, w float64) string {
	if b.boosts == nil {
		b.boosts = make(map[string]string)
	}
	if p, ok := b.boosts[field]; ok {
		return p
	}
	p := b.param(float32(w))
	b.boosts[field] = p
	return p
}

// fieldClause compiles a clause against one field. field is one of the
// field constants, never input.
func (b *keywordBuilder) fieldClause(field string, c types.KeywordClause, text string) string {
	all := c.Mode == types.KeywordAll
	switch {
	case c.Mode == types.KeywordPhrase:
		// pg_search rejects a phrase of fewer than two terms, so a
		// one-term phrase falls back to a match of that term.
		tokens := fmt.Sprintf(termsSQL, text)
		return fmt.Sprintf(
			"(CASE WHEN cardinality(%s) > 1 THEN paradedb.phrase('%s', %s, slop => %s::int) ELSE paradedb.match('%s', %s) END)",
			tokens, field, tokens, b.param(c.Slop), field, text)
	case c.Fuzziness > 0:
		return fmt.Sprintf(
			"paradedb.match('%s', %s, distance => %s::int, transposition_cost_one => %t, prefix => %t, conjunction_mode => %t)",
			field, text, b.param(c.Fuzziness), c.Transpositions, c.Prefix, all)
	case c.Prefix:
		// pdb.match with prefix needs a distance of at least 1; an exact
		// prefix is a fuzzy_term of distance 0 per term.
		group := "should"
		if all {
			group = "must"
		}
		return fmt.Sprintf(
			"paradedb.boolean(%s => ARRAY(SELECT paradedb.fuzzy_term('%s', tok, 0, false, true) FROM unnest(%s) AS tok))",
			group, field, fmt.Sprintf(termsSQL, text))
	case all:
		return fmt.Sprintf("paradedb.match('%s', %s, conjunction_mode => true)", field, text)
	default:
		return fmt.Sprintf("paradedb.match('%s', %s)", field, text)
	}
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// newHighlight builds a hit's highlight from pg_search's snippet and its
// [start, end) byte positions. Positions outside the text are dropped. It
// returns nil when there is neither a snippet nor a span, as for a match
// outside the variant text.
func newHighlight(snippet *string, positions [][]int32, textLen int) *types.Highlight {
	h := &types.Highlight{}
	if snippet != nil {
		h.Snippet = *snippet
	}
	for _, p := range positions {
		if len(p) != 2 || p[0] < 0 || p[0] >= p[1] || int(p[1]) > textLen {
			continue
		}
		h.Spans = append(h.Spans, types.TextSpan{Start: int(p[0]), End: int(p[1])})
	}
	if h.Snippet == "" && len(h.Spans) == 0 {
		return nil
	}
	return h
}

// SearchByKeyword performs BM25 full-text search over variant text with
// ParadeDB pg_search. It applies the same scope, time-range, content-type,
// and metadata filters as SearchByEmbedding, in SQL, before the limit.
// Scores are BM25 scores, not cosine similarities, so a MinScore tuned for
// vector search does not carry over.
//
// Without opts.Keyword, query is plain text and any of its terms matches
// the variant text. With it, the search runs that structured query (see
// types.KeywordQueryFor): phrase, prefix, and fuzzy matching, boolean
// clauses, boosts on the section heading and document title, and
// highlighting. An invalid query returns an error wrapping
// types.ErrInvalidKeywordQuery. In either case input is plain text: it is
// bound as a parameter and tokenized, never parsed as query syntax.
//
// The BM25 index lives in Postgres next to the rows it indexes, so inserts,
// ReplaceDocument, and DeleteDocument update it in the same transaction and
// every process sees the same index. A query without text returns no hits.
func (s *Store) SearchByKeyword(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	q := types.KeywordQueryFor(query, opts)
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("pgstore: %w", err)
	}
	if q.IsEmpty() {
		return nil, nil
	}
	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}
	sql, args := buildKeywordSQL(q, opts, limit)

	var results []types.SearchHit
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The query joins the indexed table to unindexed ones, which
		// pg_search reports as a planner warning on every search. The plan
		// is the intended one, so silence the warning for this transaction.
		if _, err := tx.Exec(ctx, `SELECT set_config('paradedb.planner_warnings', 'off', true)`); err != nil {
			return fmt.Errorf("pgstore: configure keyword search: %w", err)
		}
		var err error
		results, err = scanSearchHits(ctx, tx, sql, args, q.Highlight != nil)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: keyword search (requires the pg_search extension): %w", err)
	}
	return results, nil
}

// KeywordRetriever is a types.Retriever that runs BM25 keyword search in
// Postgres through Store.SearchByKeyword. Pair it with a vector retriever
// for hybrid search; the pipeline fuses both with its configured Fuser.
//
// Unlike bm25retriever, it keeps no in-memory index: it needs no Index,
// Remove, or RebuildIndex calls, and a new process searches documents that
// another process ingested.
type KeywordRetriever struct {
	searcher types.KeywordSearcher
}

// NewKeywordRetriever returns a BM25 retriever over a store's keyword
// search, such as a *Store.
func NewKeywordRetriever(searcher types.KeywordSearcher) *KeywordRetriever {
	return &KeywordRetriever{searcher: searcher}
}

// Retrieve returns the variants that best match query by BM25 score. It
// runs opts.Keyword when set; see Store.SearchByKeyword.
func (r *KeywordRetriever) Retrieve(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	return r.searcher.SearchByKeyword(ctx, query, opts)
}

// Name reports "bm25", the same name bm25retriever uses, so fusion weights
// keyed by name apply to either backend.
func (r *KeywordRetriever) Name() string { return "bm25" }
