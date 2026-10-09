// Package fusion provides the rank fusion strategies the RAG pipeline uses
// to merge the ranked lists of several retrievers and queries: Reciprocal
// Rank Fusion (the default) and a weighted variant that trusts some
// retrievers more than others.
package fusion

import (
	"crypto/sha256"
	"fmt"
	"math"
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

// Fuse implements types.Fuser. Per-search weights (types.FuseOptions.Weights)
// make it weighted; retrievers they do not name weigh 1.
func (f RRF) Fuse(lists []types.RankedList, opts types.FuseOptions) []types.SearchHit {
	return fuse(lists, rankConstant(opts.K, f.K), withOverrides(opts.Weights, func(string) float64 { return 1 }))
}

// MaxFusedScore is the score of a hit ranked first in every list.
func (f RRF) MaxFusedScore(lists []types.RankedList, opts types.FuseOptions) float64 {
	return rankCeiling(lists, rankConstant(opts.K, f.K), withOverrides(opts.Weights, func(string) float64 { return 1 }))
}

// ScoreNormalization selects how Weighted fuses retriever scores.
type ScoreNormalization string

const (
	// NormalizeNone fuses by rank, as weighted Reciprocal Rank Fusion. It
	// ignores the retrievers' raw scores. It is the default.
	NormalizeNone ScoreNormalization = ""
	// NormalizeMinMax rescales each list's raw scores to [0,1], the best hit
	// of the list scoring 1 and the worst 0. A list whose hits all score the
	// same gives each of them 1.
	NormalizeMinMax ScoreNormalization = "min-max"
	// NormalizeZScore rescales each list's raw scores to standard scores:
	// the distance from the list's mean in standard deviations. A list whose
	// hits all score the same gives each of them 0. Fused scores can be
	// negative.
	NormalizeZScore ScoreNormalization = "z-score"
)

// Weighted fuses the lists of several retrievers with a weight per
// retriever. Retrievers are matched by name (see types.Named). A retriever
// missing from Weights gets Default, or 1 when Default is zero; per-search
// weights (types.FuseOptions.Weights) override both. A weight <= 0 removes
// the retriever's lists from fusion.
//
// With the default NormalizeNone it is weighted Reciprocal Rank Fusion: a
// hit at zero-based rank r in a list from retriever R adds
// weight(R)/(k+r+1). With another Normalization it fuses scores instead of
// ranks: each list's raw scores are normalized, and a hit adds weight(R)
// times its normalized score, so a hit absent from a list adds nothing for
// it. K is then unused. Score fusion keeps how far apart the hits of a list
// score, which rank fusion discards, but it trusts the retrievers' score
// scales to be meaningful.
type Weighted struct {
	K             int
	Weights       map[string]float64
	Default       float64
	Normalization ScoreNormalization
}

var (
	_ types.Fuser         = Weighted{}
	_ types.FusionCeiling = Weighted{}
)

// Fuse implements types.Fuser.
func (f Weighted) Fuse(lists []types.RankedList, opts types.FuseOptions) []types.SearchHit {
	weight := withOverrides(opts.Weights, f.weight)
	if f.scoreBased() {
		return fuseScores(lists, f.Normalization, weight)
	}
	return fuse(lists, rankConstant(opts.K, f.K), weight)
}

// MaxFusedScore is the score of a hit ranked first in every weighted list.
func (f Weighted) MaxFusedScore(lists []types.RankedList, opts types.FuseOptions) float64 {
	weight := withOverrides(opts.Weights, f.weight)
	if f.scoreBased() {
		total := 0.0
		for _, l := range lists {
			w := weight(l.Retriever)
			if w <= 0 || len(l.Hits) == 0 {
				continue
			}
			norm := normalize(l.Hits, f.Normalization)
			total += w * max(slicesMax(norm), 0)
		}
		return total
	}
	return rankCeiling(lists, rankConstant(opts.K, f.K), weight)
}

// scoreBased reports whether f fuses normalized scores. An unknown
// Normalization fuses by rank, like NormalizeNone.
func (f Weighted) scoreBased() bool {
	return f.Normalization == NormalizeMinMax || f.Normalization == NormalizeZScore
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

// withOverrides returns a weight function that prefers the per-search
// weight of a retriever and falls back to base.
func withOverrides(overrides map[string]float64, base func(string) float64) func(string) float64 {
	if len(overrides) == 0 {
		return base
	}
	return func(retriever string) float64 {
		if w, ok := overrides[retriever]; ok {
			return max(w, 0)
		}
		return base(retriever)
	}
}

// rankCeiling is the fused rank score of a hit ranked first in every list.
func rankCeiling(lists []types.RankedList, k int, weight func(string) float64) float64 {
	total := 0.0
	for _, l := range lists {
		total += weight(l.Retriever)
	}
	return total / float64(k+1)
}

// fuseScores sums weighted normalized scores per Key, merging duplicates
// like fuse.
func fuseScores(lists []types.RankedList, mode ScoreNormalization, weight func(string) float64) []types.SearchHit {
	scores := make(map[string]float64)
	hits := make(map[string]types.SearchHit)
	var order []string
	for _, list := range lists {
		w := weight(list.Retriever)
		if w <= 0 || len(list.Hits) == 0 {
			continue
		}
		norm := normalize(list.Hits, mode)
		// A key seen twice in one list (two children of one expanded
		// section) counts once, at its best normalized score.
		best := make(map[string]float64, len(list.Hits))
		for i, hit := range list.Hits {
			key := Key(&hit)
			if prev, ok := best[key]; !ok || norm[i] > prev {
				best[key] = norm[i]
			}
			existing, ok := hits[key]
			if !ok {
				hits[key] = hit
				order = append(order, key)
				continue
			}
			hits[key] = MergeDuplicate(existing, hit)
		}
		for key, s := range best {
			scores[key] += w * s
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

// normalize returns the normalized raw scores of hits, in hit order.
func normalize(hits []types.SearchHit, mode ScoreNormalization) []float64 {
	out := make([]float64, len(hits))
	switch mode {
	case NormalizeMinMax:
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, h := range hits {
			lo, hi = min(lo, h.Score), max(hi, h.Score)
		}
		for i, h := range hits {
			if hi > lo {
				out[i] = (h.Score - lo) / (hi - lo)
			} else {
				out[i] = 1
			}
		}
	case NormalizeZScore:
		mean := 0.0
		for _, h := range hits {
			mean += h.Score
		}
		mean /= float64(len(hits))
		variance := 0.0
		for _, h := range hits {
			variance += (h.Score - mean) * (h.Score - mean)
		}
		std := math.Sqrt(variance / float64(len(hits)))
		for i, h := range hits {
			if std > 0 {
				out[i] = (h.Score - mean) / std
			}
		}
	}
	return out
}

func slicesMax(xs []float64) float64 {
	m := math.Inf(-1)
	for _, x := range xs {
		m = max(m, x)
	}
	return m
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
	if best.Highlight == nil && other.Highlight != nil && best.Variant.UUID == other.Variant.UUID &&
		best.Provenance.ExpandedFromVariantUUID == other.Provenance.ExpandedFromVariantUUID {
		best.Highlight = other.Highlight
	}
	return best
}
