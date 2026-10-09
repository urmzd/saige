// Package rank holds the Selector seam and the BM25 ranker without any
// dependency on the agent loop, so the agent package itself can rank
// transcripts and catalogs. Package selector re-exports everything here;
// most callers should import selector instead.
package rank

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"unicode"
)

// ErrEmptyQuery is returned when a query has no searchable terms.
var ErrEmptyQuery = errors.New("selector: query has no searchable terms")

// Selector returns up to k items from items that best match query, best
// first. k <= 0 returns every match. Items that do not match at all are
// omitted, so the result can be shorter than k or empty. Implementations
// must not modify items and must be safe for concurrent use.
type Selector[T any] interface {
	Select(ctx context.Context, query string, items []T, k int) ([]T, error)
}

// SelectorFunc adapts a function to Selector.
type SelectorFunc[T any] func(ctx context.Context, query string, items []T, k int) ([]T, error)

// Select implements Selector.
func (f SelectorFunc[T]) Select(ctx context.Context, query string, items []T, k int) ([]T, error) {
	return f(ctx, query, items, k)
}

// BM25 ranks items by Okapi BM25 over the text that Text extracts from each
// item. The index is built per call from the items given, so it always
// reflects the current catalog and holds no state between calls. That suits
// catalogs of hundreds or a few thousand entries; a larger corpus belongs
// in a persistent retriever.
//
// Ties keep the input order, so ranking is deterministic.
type BM25[T any] struct {
	// Text returns the searchable text of an item. Required.
	Text func(T) string
	// K1 controls term-frequency saturation. Zero uses 1.2.
	K1 float64
	// B controls document-length normalization. Zero uses 0.75; use a
	// small positive value to nearly disable it.
	B float64
}

// NewBM25 returns a BM25 selector with the standard parameters.
func NewBM25[T any](text func(T) string) *BM25[T] {
	return &BM25[T]{Text: text}
}

// Select implements Selector.
func (s *BM25[T]) Select(ctx context.Context, query string, items []T, k int) ([]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Text == nil {
		return nil, errors.New("selector: BM25 needs a Text function")
	}
	terms := uniqueTerms(Tokenize(query))
	if len(terms) == 0 {
		return nil, ErrEmptyQuery
	}
	if len(items) == 0 {
		return nil, nil
	}
	k1, b := s.K1, s.B
	if k1 == 0 {
		k1 = 1.2
	}
	if b == 0 {
		b = 0.75
	}

	freqs := make([]map[string]int, len(items))
	lengths := make([]float64, len(items))
	df := make(map[string]int, len(terms))
	var total float64
	for i, item := range items {
		tokens := Tokenize(s.Text(item))
		tf := make(map[string]int, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		for _, term := range terms {
			if tf[term] > 0 {
				df[term]++
			}
		}
		freqs[i] = tf
		lengths[i] = float64(len(tokens))
		total += lengths[i]
	}
	n := float64(len(items))
	avg := total / n
	if avg == 0 {
		avg = 1
	}

	type scored struct {
		index int
		score float64
	}
	var hits []scored
	for i, tf := range freqs {
		var score float64
		for _, term := range terms {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			d := float64(df[term])
			idf := math.Log(1 + (n-d+0.5)/(d+0.5))
			score += idf * f * (k1 + 1) / (f + k1*(1-b+b*lengths[i]/avg))
		}
		if score > 0 {
			hits = append(hits, scored{index: i, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	out := make([]T, len(hits))
	for i, h := range hits {
		out[i] = items[h.index]
	}
	return out, nil
}

// Tokenize lowercases text and splits it into letter and digit runs. It also
// splits camelCase and snake_case identifiers, so a query for "file" matches
// a tool named read_file or readFile.
func Tokenize(text string) []string {
	var (
		tokens []string
		cur    []rune
		prev   rune
	)
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return tokens
}

func uniqueTerms(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	out := tokens[:0:0]
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
