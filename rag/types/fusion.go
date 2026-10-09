package types

// Named is an optional interface for pipeline components (retrievers,
// embedders) that report a stable name. The pipeline uses it to label
// retrieval stats, telemetry, and per-retriever fusion weights.
type Named interface {
	Name() string
}

// RankedList is one retriever's ranked hits for one query, as fusion sees it.
type RankedList struct {
	// Retriever is the retriever's name (see Named), or "retriever-N".
	Retriever string
	// QueryIndex is the position of the query in the transformed query list.
	QueryIndex int
	Hits       []SearchHit
}

// FuseOptions carries per-search settings to a Fuser.
type FuseOptions struct {
	// K is the rank constant set by WithFusionK. Zero means the fuser's
	// default.
	K int
	// Weights overrides the fuser's weight for each named retriever (see
	// WithFusionWeights). Retrievers it does not name keep the fuser's
	// weight. A weight <= 0 leaves the retriever's lists out.
	Weights map[string]float64
}

// Fuser merges the ranked lists of every retriever and query into one list
// of distinct hits with fused scores. The pipeline then applies recency,
// sorts by score, applies content dedup and MinScore, and cuts the list to
// the candidate pool, so a Fuser need not sort or truncate.
//
// Implementations must merge hits that denote the same content into one
// hit; the fusion package provides the default identity (fusion.Key).
type Fuser interface {
	Fuse(lists []RankedList, opts FuseOptions) []SearchHit
}

// FusionCeiling is an optional Fuser interface that reports the highest
// fused score a hit could reach for the given lists. WithMinScore divides
// each fused score by it so the threshold is in [0,1]. Without it the
// pipeline divides by the best fused score of the search.
type FusionCeiling interface {
	MaxFusedScore(lists []RankedList, opts FuseOptions) float64
}
