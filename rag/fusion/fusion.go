// Package fusion provides the rank fusion strategies the RAG pipeline uses
// to merge the ranked lists of several retrievers and queries: Reciprocal
// Rank Fusion (the default) and a weighted variant that trusts some
// retrievers more than others.
package fusion

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/urmzd/saige/rag/types"
)

// DefaultK is the Reciprocal Rank Fusion constant used when neither the
// fuser nor the search sets one.
const DefaultK = 60

// RRF is Reciprocal Rank Fusion: a hit at zero-based rank r in a list adds
// 1/(k+r+1) to its fused score, summed over every list that contains it.
type RRF struct {
	// K is the rank constant. A per-search WithFusionK takes precedence;
	// zero means DefaultK.
	K int
}

var (
	_ types.Fuser         = RRF{}
	_ types.FusionCeiling = RRF{}
)

// Fuse implements types.Fuser.
func (f RRF) Fuse(lists []types.RankedList, opts types.FuseOptions) []types.SearchHit {
	return fuse(lists, rankConstant(opts.K, f.K), func(string) float64 { return 1 })
}

// MaxFusedScore is the score of a hit ranked first in every list.
func (f RRF) MaxFusedScore(lists []types.RankedList, opts types.FuseOptions) float64 {
	return float64(len(lists)) / float64(rankConstant(opts.K, f.K)+1)
}

// Weighted is Reciprocal Rank Fusion with a weight per retriever: a hit at
// rank r in a list from retriever R adds Weights[R]/(k+r+1). Retrievers are
// matched by name (see types.Named). A retriever missing from Weights gets
// Default, or 1 when Default is zero. A weight <= 0 removes the retriever's
// lists from fusion.
type Weighted struct {
	K       int
	Weights map[string]float64
	Default float64
}

var (
	_ types.Fuser         = Weighted{}
	_ types.FusionCeiling = Weighted{}
)

// Fuse implements types.Fuser.
func (f Weighted) Fuse(lists []types.RankedList, opts types.FuseOptions) []types.SearchHit {
	return fuse(lists, rankConstant(opts.K, f.K), f.weight)
}

// MaxFusedScore is the score of a hit ranked first in every weighted list.
func (f Weighted) MaxFusedScore(lists []types.RankedList, opts types.FuseOptions) float64 {
	total := 0.0
	for _, l := range lists {
		total += f.weight(l.Retriever)
	}
	return total / float64(rankConstant(opts.K, f.K)+1)
}

func (f Weighted) weight(retriever string) float64 {
	w, ok := f.Weights[retriever]
	if !ok {
		w = f.Default
		if w == 0 {
			w = 1
		}
	}
	return max(w, 0)
}

func rankConstant(perSearch, configured int) int {
	switch {
	case perSearch > 0:
		return perSearch
	case configured > 0:
		return configured
	default:
		return DefaultK
	}
}

// fuse sums weighted reciprocal-rank contributions per Key and merges the
// hits that share a key with MergeDuplicate. The result is in first-seen
// order; the pipeline sorts it.
func fuse(lists []types.RankedList, k int, weight func(string) float64) []types.SearchHit {
	scores := make(map[string]float64)
	hits := make(map[string]types.SearchHit)
	var order []string
	for _, list := range lists {
		w := weight(list.Retriever)
		if w <= 0 {
			continue
		}
		for rank, hit := range list.Hits {
			key := Key(&hit)
			scores[key] += w / float64(k+rank+1)
			existing, ok := hits[key]
			if !ok {
				hits[key] = hit
				order = append(order, key)
				continue
			}
			hits[key] = MergeDuplicate(existing, hit)
		}
	}
	out := make([]types.SearchHit, 0, len(order))
	for _, key := range order {
		hit := hits[key]
		hit.Score = scores[key]
		out = append(out, hit)
	}
	return out
}

// Key returns the identity under which fusion merges a hit. Hits whose text
// was expanded to a parent section merge by section, so two children of one
// section that reach fusion from different retrievers or queries become a
// single result. An expanded hit without a section UUID falls back to a hash
// of its trimmed text. Every other hit merges by variant UUID.
func Key(hit *types.SearchHit) string {
	if hit.Provenance.ExpandedFromVariantUUID == "" {
		return "variant:" + hit.Variant.UUID
	}
	if hit.Provenance.SectionUUID != "" {
		return "section:" + hit.Provenance.SectionUUID
	}
	return fmt.Sprintf("text:%x", sha256.Sum256([]byte(strings.TrimSpace(hit.Variant.Text))))
}

// ContentKey returns a hash of what a hit shows a reader: its content type
// plus its trimmed text, or its bytes when it has no text. Two hits with the
// same ContentKey display the same passage.
func ContentKey(hit *types.SearchHit) string {
	h := sha256.New()
	h.Write([]byte(hit.Variant.ContentType))
	h.Write([]byte{0})
	if text := strings.TrimSpace(hit.Variant.Text); text != "" {
		h.Write([]byte(text))
	} else {
		h.Write(hit.Variant.Data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// MergeDuplicate combines two hits that share a Key. The hit with the higher
// raw score represents the pair, and fields it lacks are taken from the
// other: retrievers differ in what they populate, so a lexical hit without
// an embedding or timestamp must not erase what a vector hit for the same
// content carried.
func MergeDuplicate(a, b types.SearchHit) types.SearchHit {
	best, other := a, b
	if b.Score > a.Score {
		best, other = b, a
	}
	if len(best.Variant.Embedding) == 0 && len(other.Variant.Embedding) > 0 && best.Variant.UUID == other.Variant.UUID {
		best.Variant.Embedding = other.Variant.Embedding
	}
	if best.Timestamp.IsZero() {
		best.Timestamp = other.Timestamp
	}
	if best.Provenance.DocumentTitle == "" {
		best.Provenance.DocumentTitle = other.Provenance.DocumentTitle
	}
	if best.Provenance.SourceURI == "" {
		best.Provenance.SourceURI = other.Provenance.SourceURI
	}
	return best
}
