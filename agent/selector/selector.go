// Package selector ranks items against a query so an agent can disclose a
// few relevant tools, skills, or records instead of all of them.
//
// Selector[T] is the seam: tool search, skill search, memory recall, and
// sub-agent transcript search all take one. BM25 is the default
// implementation. It is lexical, needs no model or network, and ranks
// deterministically, so the same catalog and query always produce the same
// disclosure (which a durable replay needs).
//
// The ranking code lives in package rank, which has no dependency on the
// agent loop, so the agent package can use it too. The names here are
// aliases of that package.
package selector

import (
	"github.com/urmzd/saige/agent/selector/rank"
)

// ErrEmptyQuery is returned when a query has no searchable terms.
var ErrEmptyQuery = rank.ErrEmptyQuery

// Selector returns up to k items from items that best match query, best
// first. k <= 0 returns every match. Items that do not match at all are
// omitted, so the result can be shorter than k or empty. Implementations
// must not modify items and must be safe for concurrent use.
type Selector[T any] = rank.Selector[T]

// SelectorFunc adapts a function to Selector.
type SelectorFunc[T any] = rank.SelectorFunc[T]

// BM25 ranks items by Okapi BM25 over the text that Text extracts from each
// item. See rank.BM25.
type BM25[T any] = rank.BM25[T]

// NewBM25 returns a BM25 selector with the standard parameters.
func NewBM25[T any](text func(T) string) *BM25[T] {
	return rank.NewBM25(text)
}

// Tokenize lowercases text and splits it into letter and digit runs. It also
// splits camelCase and snake_case identifiers, so a query for "file" matches
// a tool named read_file or readFile.
func Tokenize(text string) []string {
	return rank.Tokenize(text)
}
