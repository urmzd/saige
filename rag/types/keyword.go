package types

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// KeywordMode selects how the terms of a keyword clause match.
type KeywordMode string

const (
	// KeywordAny matches a variant that contains any of the clause's terms.
	// It is the default and the behavior of a plain-text keyword search.
	KeywordAny KeywordMode = ""
	// KeywordAll matches a variant that contains every term of the clause.
	KeywordAll KeywordMode = "all"
	// KeywordPhrase matches a variant that contains the clause's terms
	// next to each other and in order, within KeywordClause.Slop. A phrase
	// of one term matches like KeywordAny.
	KeywordPhrase KeywordMode = "phrase"
)

// MaxKeywordFuzziness is the largest edit distance a keyword clause may
// allow. ParadeDB pg_search rejects larger distances.
const MaxKeywordFuzziness = 2

// MaxKeywordBoost bounds a field boost. ParadeDB pg_search accepts boosts up
// to 2048.
const MaxKeywordBoost = 2048

// ErrInvalidKeywordQuery reports a KeywordQuery that cannot be run, such as a
// fuzzy phrase or an out-of-range boost.
var ErrInvalidKeywordQuery = errors.New("invalid keyword query")

// KeywordClause is one lexical condition. Text is always plain text: it is
// split into terms with the index's tokenizer and never parsed as query
// syntax, so characters such as quotes, colons, or AND are ordinary text.
type KeywordClause struct {
	// Text is the clause's plain-text input.
	Text string `json:"text"`
	// Mode selects any-term, all-terms, or phrase matching.
	Mode KeywordMode `json:"mode,omitempty"`
	// Prefix makes each term match any indexed term that starts with it,
	// so "postg" matches "postgres". It applies to KeywordAny and
	// KeywordAll.
	Prefix bool `json:"prefix,omitempty"`
	// Fuzziness is the edit distance, 0 to MaxKeywordFuzziness, within
	// which a term still matches, so 1 lets "okapy" match "okapi". It
	// applies to KeywordAny and KeywordAll.
	Fuzziness int `json:"fuzziness,omitempty"`
	// Transpositions counts a swap of two adjacent characters as one edit
	// instead of two. It applies only when Fuzziness is set.
	Transpositions bool `json:"transpositions,omitempty"`
	// Slop is how many position moves a phrase may need to match, such as
	// one extra word in between (1) or two swapped words (2). It applies to
	// KeywordPhrase.
	Slop int `json:"slop,omitempty"`
}

// FieldBoosts weights the indexed fields of a variant in a keyword search.
// A field with weight 0 is not searched; a weight above 1 makes a match in
// that field count for more of the score. A clause matches when it matches
// in any searched field, and each field is matched on its own: KeywordAll
// needs every term in one field, and a phrase cannot span two fields.
type FieldBoosts struct {
	// Body weights the variant's own text.
	Body float64 `json:"body"`
	// Title weights the title of the variant's document.
	Title float64 `json:"title"`
	// Heading weights the heading of the variant's section.
	Heading float64 `json:"heading"`
}

// DefaultFieldBoosts searches only the variant text, as a plain keyword
// search does.
var DefaultFieldBoosts = FieldBoosts{Body: 1}

// HighlightOptions asks a keyword search for the matched part of each hit's
// text (see SearchHit.Highlight). Empty fields take the defaults.
type HighlightOptions struct {
	// StartTag and EndTag enclose each matched term in the snippet. The
	// defaults are "<b>" and "</b>".
	StartTag string `json:"start_tag,omitempty"`
	EndTag   string `json:"end_tag,omitempty"`
	// MaxChars bounds the snippet length in characters. Zero means 150.
	MaxChars int `json:"max_chars,omitempty"`
}

// Highlight defaults.
const (
	DefaultHighlightStartTag = "<b>"
	DefaultHighlightEndTag   = "</b>"
	DefaultHighlightMaxChars = 150
)

// WithDefaults returns o with empty fields set to the defaults.
func (o HighlightOptions) WithDefaults() HighlightOptions {
	if o.StartTag == "" {
		o.StartTag = DefaultHighlightStartTag
	}
	if o.EndTag == "" {
		o.EndTag = DefaultHighlightEndTag
	}
	if o.MaxChars <= 0 {
		o.MaxChars = DefaultHighlightMaxChars
	}
	return o
}

// KeywordQuery is a structured lexical query. Its zero value with Text set
// is a plain-text search: Text is tokenized like the indexed text and a
// variant that contains any term matches.
//
// Text, Mode, Prefix, Fuzziness, Transpositions, and Slop describe the
// main clause (see KeywordClause), which every hit must match. Must adds clauses every hit must also match, MustNot excludes
// variants that match any of its clauses, and Should adds clauses that only
// raise the score of variants that match them. A query whose main clause
// and Must are both empty returns the variants that match at least one
// Should clause.
//
// User input belongs in the Text fields only. Stores pass every value as a
// bind parameter and never splice it into query syntax.
type KeywordQuery struct {
	// Text is the main clause's text. Empty means the search query text
	// when the query is passed through SearchOptions.Keyword.
	Text           string      `json:"text,omitempty"`
	Mode           KeywordMode `json:"mode,omitempty"`
	Prefix         bool        `json:"prefix,omitempty"`
	Fuzziness      int         `json:"fuzziness,omitempty"`
	Transpositions bool        `json:"transpositions,omitempty"`
	Slop           int         `json:"slop,omitempty"`

	Must    []KeywordClause `json:"must,omitempty"`
	Should  []KeywordClause `json:"should,omitempty"`
	MustNot []KeywordClause `json:"must_not,omitempty"`

	// Fields weights the indexed fields. Nil means DefaultFieldBoosts, the
	// variant text alone. Stores that index only the variant text, such as
	// the in-memory BM25 index, search Body and ignore the other weights.
	Fields *FieldBoosts `json:"fields,omitempty"`
	// Highlight, when set, asks for the matched part of each hit's text.
	// Stores that cannot highlight leave SearchHit.Highlight nil.
	Highlight *HighlightOptions `json:"highlight,omitempty"`
}

// PlainKeywordQuery returns the default query for text: any term matches,
// in the variant text only.
func PlainKeywordQuery(text string) KeywordQuery {
	return KeywordQuery{Text: text}
}

// Main returns the query's main clause.
func (q KeywordQuery) Main() KeywordClause {
	return KeywordClause{
		Text: q.Text, Mode: q.Mode, Prefix: q.Prefix,
		Fuzziness: q.Fuzziness, Transpositions: q.Transpositions, Slop: q.Slop,
	}
}

// Boosts returns the field weights, DefaultFieldBoosts when Fields is nil.
func (q KeywordQuery) Boosts() FieldBoosts {
	if q.Fields == nil {
		return DefaultFieldBoosts
	}
	return *q.Fields
}

// IsEmpty reports whether the query has no positive clause with text, so it
// can match nothing. A query with only MustNot clauses is empty.
func (q KeywordQuery) IsEmpty() bool {
	if !blank(q.Text) {
		return false
	}
	for _, c := range q.Must {
		if !blank(c.Text) {
			return false
		}
	}
	for _, c := range q.Should {
		if !blank(c.Text) {
			return false
		}
	}
	return true
}

// Validate reports the first setting that no store can run, wrapped in
// ErrInvalidKeywordQuery.
func (q KeywordQuery) Validate() error {
	if err := q.Main().validate("query"); err != nil {
		return err
	}
	for _, group := range []struct {
		name    string
		clauses []KeywordClause
	}{{"must", q.Must}, {"should", q.Should}, {"must_not", q.MustNot}} {
		for i, c := range group.clauses {
			if err := c.validate(fmt.Sprintf("%s[%d]", group.name, i)); err != nil {
				return err
			}
		}
	}
	b := q.Boosts()
	for _, f := range []struct {
		name string
		w    float64
	}{{"body", b.Body}, {"title", b.Title}, {"heading", b.Heading}} {
		if math.IsNaN(f.w) || f.w < 0 || f.w > MaxKeywordBoost {
			return fmt.Errorf("%w: %s boost %v must be in [0, %d]", ErrInvalidKeywordQuery, f.name, f.w, MaxKeywordBoost)
		}
	}
	if b.Body == 0 && b.Title == 0 && b.Heading == 0 {
		return fmt.Errorf("%w: every field boost is zero", ErrInvalidKeywordQuery)
	}
	if h := q.Highlight; h != nil && h.MaxChars < 0 {
		return fmt.Errorf("%w: highlight max chars %d is negative", ErrInvalidKeywordQuery, h.MaxChars)
	}
	return nil
}

func (c KeywordClause) validate(where string) error {
	switch c.Mode {
	case KeywordAny, KeywordAll:
		if c.Slop != 0 {
			return fmt.Errorf("%w: %s: slop applies only to phrase mode", ErrInvalidKeywordQuery, where)
		}
	case KeywordPhrase:
		if c.Prefix || c.Fuzziness != 0 {
			return fmt.Errorf("%w: %s: phrase mode does not support prefix or fuzzy matching", ErrInvalidKeywordQuery, where)
		}
		if c.Slop < 0 {
			return fmt.Errorf("%w: %s: slop %d is negative", ErrInvalidKeywordQuery, where, c.Slop)
		}
	default:
		return fmt.Errorf("%w: %s: unknown mode %q", ErrInvalidKeywordQuery, where, c.Mode)
	}
	if c.Fuzziness < 0 || c.Fuzziness > MaxKeywordFuzziness {
		return fmt.Errorf("%w: %s: fuzziness %d must be in [0, %d]", ErrInvalidKeywordQuery, where, c.Fuzziness, MaxKeywordFuzziness)
	}
	if c.Transpositions && c.Fuzziness == 0 {
		return fmt.Errorf("%w: %s: transpositions need fuzziness", ErrInvalidKeywordQuery, where)
	}
	return nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// Highlight is the part of a hit's text that matched a keyword query, for
// citations. Stores fill it only when the search asked for it (see
// KeywordQuery.Highlight) and the match was in the variant text; a match in
// the title or heading alone, or a fuzzy or prefix match, has none.
type Highlight struct {
	// Snippet is the best-matching fragment of the text, with each matched
	// term enclosed in the requested tags.
	Snippet string `json:"snippet,omitempty"`
	// Spans are the byte offsets of the matched terms in Variant.Text.
	// Parent and neighbor expansion move them so they still point into the
	// widened text.
	Spans []TextSpan `json:"spans,omitempty"`
}

// TextSpan is a half-open byte range [Start, End) of a text.
type TextSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Shifted returns a copy of h whose spans are moved by offset bytes, for a
// hit whose text now starts offset bytes before its matched text. A nil h
// stays nil.
func (h *Highlight) Shifted(offset int) *Highlight {
	if h == nil {
		return nil
	}
	out := &Highlight{Snippet: h.Snippet}
	if len(h.Spans) > 0 {
		out.Spans = make([]TextSpan, len(h.Spans))
		for i, s := range h.Spans {
			out.Spans[i] = TextSpan{Start: s.Start + offset, End: s.End + offset}
		}
	}
	return out
}
