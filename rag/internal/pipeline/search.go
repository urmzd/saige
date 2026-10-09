package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/rag/contextassembler"
	"github.com/urmzd/saige/rag/fusion"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// ln2 is the natural logarithm of 2, used for half-life exponential decay.
const ln2 = 0.6931471805599453

// applyRecency multiplies each hit's fused score by an exponential time-decay
// factor blended by the configured weight. It is a no-op unless WithRecency
// supplied a positive half-life, preserving the default (recency-free) ranking.
func applyRecency(hits []ragtypes.SearchHit, cfg *ragtypes.SearchConfig) {
	if cfg.RecencyHalfLife <= 0 {
		return
	}
	weight := cfg.RecencyWeight
	if weight < 0 {
		weight = 0
	}
	if weight > 1 {
		weight = 1
	}
	now := cfg.RecencyNow
	if now.IsZero() {
		now = time.Now()
	}
	halfLife := float64(cfg.RecencyHalfLife)
	for i := range hits {
		recency := recencyFactor(hits[i].Timestamp, now, halfLife)
		hits[i].Score = (1-weight)*hits[i].Score + weight*hits[i].Score*recency
	}
}

// recencyFactor returns exp(-ln2 * age / halfLife) in (0,1]. A zero timestamp
// (unknown age) or non-positive age yields 1.0 (no decay).
func recencyFactor(ts, now time.Time, halfLife float64) float64 {
	if ts.IsZero() || halfLife <= 0 {
		return 1.0
	}
	age := now.Sub(ts)
	if age <= 0 {
		return 1.0
	}
	return math.Exp(-ln2 * float64(age) / halfLife)
}

func (p *pipelineImpl) Search(ctx context.Context, query string, opts ...ragtypes.SearchOption) (result *ragtypes.SearchPipelineResult, err error) {
	cfg := &ragtypes.SearchConfig{Limit: 10}
	for _, o := range opts {
		o(cfg)
	}

	if len(p.cfg.Retrievers) == 0 {
		return nil, fmt.Errorf("%w", ragtypes.ErrNoRetriever)
	}
	scope, err := p.scopeFor(cfg.Scope)
	if err != nil {
		return nil, err
	}
	cfg.Scope = scope

	ctx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanSearch, ragtypes.Attr(ragtypes.AttrScope, scope))
	defer func() {
		if result != nil {
			span.SetAttributes(ragtypes.Attr(ragtypes.AttrHits, len(result.Hits)))
		}
		if errors.Is(err, ragtypes.ErrPartialSearch) {
			span.SetAttributes(ragtypes.Attr(ragtypes.AttrPartial, true))
		}
		endSpan(span, err)
	}()

	var partial []error

	// Step 1: Query transformation. A failed or partial expansion degrades
	// to the queries it did produce, or to the raw query, unless the context
	// itself is done.
	queries := []string{query}
	if p.cfg.QueryTransformer != nil {
		stageCtx, stage := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanTransform)
		transformed, err := p.cfg.QueryTransformer.Transform(stageCtx, query)
		if len(transformed) > 0 {
			stage.SetAttributes(ragtypes.Attr(ragtypes.AttrQueries, len(transformed)))
		}
		endSpan(stage, err)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("transform query: %w", errors.Join(ctxErr, err))
			}
			p.cfg.Logger.WarnContext(ctx, "query transform failed; continuing with available queries", "error", err)
			partial = append(partial, fmt.Errorf("transform query: %w", err))
		}
		if len(transformed) > 0 {
			queries = transformed
		}
	}

	// Step 2: Retrieve a candidate pool from every retriever for every query.
	candidateK := candidatePoolSize(cfg, p.cfg.Reranker != nil)
	searchOpts := &ragtypes.SearchOptions{
		ContentTypes:    cfg.ContentTypes,
		Limit:           candidateK,
		MetadataFilters: cfg.MetadataFilters,
		Scope:           cfg.Scope,
		Since:           cfg.Since,
		Until:           cfg.Until,
		Keyword:         cfg.Keyword,
	}

	lists, stats, retrieveErr, err := p.retrieveAll(ctx, queries, searchOpts)
	if err != nil {
		return nil, err
	}
	if retrieveErr != nil {
		partial = append(partial, retrieveErr)
	}

	// Step 3: Fusion, recency blending, content dedup, MinScore filter, and
	// truncation to the candidate pool.
	_, stage := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanFuse)
	merged := p.fuse(lists, cfg, candidateK)
	stage.SetAttributes(ragtypes.Attr(ragtypes.AttrCandidates, len(merged)))
	endSpan(stage, nil)

	// Step 4: Rerank the pool, then cut it to the requested page.
	if p.cfg.Reranker != nil && len(merged) > 0 {
		stageCtx, stage := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanRerank,
			ragtypes.Attr(ragtypes.AttrCandidates, len(merged)))
		reranked, err := p.cfg.Reranker.Rerank(stageCtx, query, merged)
		endSpan(stage, err)
		if err != nil {
			return nil, fmt.Errorf("rerank: %w", err)
		}
		merged = reranked
	}
	if cfg.Limit > 0 && len(merged) > cfg.Limit {
		merged = merged[:cfg.Limit]
	}

	// Step 5: Neighbor window expansion of the final page.
	if cfg.NeighborWindow > 0 && len(merged) > 0 {
		stageCtx, stage := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanExpand)
		expanded, err := p.expandNeighbors(stageCtx, merged, cfg.NeighborWindow)
		endSpan(stage, err)
		if err != nil {
			partial = append(partial, err)
		}
		merged = expanded
	}

	// Step 6: Context assembly.
	result = &ragtypes.SearchPipelineResult{
		Query:      query,
		Hits:       merged,
		Retrievals: stats,
	}
	if len(queries) > 1 {
		result.TransformedQueries = queries
	}

	if cfg.AssembleContext && len(merged) > 0 {
		assembler := p.cfg.ContextAssembler
		if assembler == nil {
			assembler = &contextassembler.DefaultAssembler{MaxTokens: cfg.MaxTokens}
		}
		stageCtx, stage := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanAssemble)
		assembled, err := assembler.Assemble(stageCtx, query, merged)
		endSpan(stage, err)
		if err != nil {
			return nil, fmt.Errorf("assemble context: %w", err)
		}
		result.Context = assembled
	}

	if len(partial) > 0 {
		return result, fmt.Errorf("%w: %w", ragtypes.ErrPartialSearch, errors.Join(partial...))
	}
	return result, nil
}

// candidatePoolSize returns how many fused candidates a search keeps before
// the final cut to cfg.Limit. Rerankers only reorder what they receive, so
// with a reranker the default pool is several times the page size.
func candidatePoolSize(cfg *ragtypes.SearchConfig, reranking bool) int {
	k := cfg.CandidateK
	if k <= 0 {
		k = cfg.Limit
		if reranking {
			k = max(4*cfg.Limit, 20)
		}
	}
	return max(k, cfg.Limit)
}

// retrieveAll fans out every retriever × query pair in parallel. Retriever
// failures are tolerated as long as at least one retrieval succeeds: the
// search continues with the successful lists and the failures come back as a
// non-nil partial error. Only when ALL retrievals fail is the final error
// non-nil. A retriever that returns hits together with an error wrapping
// ragtypes.ErrPartialSearch counts as a success whose error is reported.
//
// The returned lists hold one entry per successful retrieval, so their count
// is the number of ranked lists that fusion combines. The stats hold one
// entry per pair, successful or not, in retriever then query order.
func (p *pipelineImpl) retrieveAll(ctx context.Context, queries []string, searchOpts *ragtypes.SearchOptions) (lists []ragtypes.RankedList, stats []ragtypes.RetrievalStat, partialErr, err error) {
	totalPairs := len(p.cfg.Retrievers) * len(queries)
	allLists := make([]ragtypes.RankedList, totalPairs)
	stats = make([]ragtypes.RetrievalStat, totalPairs)
	retrieveErrs := make([]error, totalPairs)
	failed := make([]bool, totalPairs)

	var wg sync.WaitGroup
	for ri, retriever := range p.cfg.Retrievers {
		name := p.names[ri]
		for qi, q := range queries {
			idx := ri*len(queries) + qi
			wg.Add(1)
			go func() {
				defer wg.Done()
				callCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanRetrieve,
					ragtypes.Attr(ragtypes.AttrRetriever, name),
					ragtypes.Attr(ragtypes.AttrQueryIndex, qi))
				start := time.Now()
				hits, err := retriever.Retrieve(callCtx, q, searchOpts)
				elapsed := time.Since(start)
				span.SetAttributes(ragtypes.Attr(ragtypes.AttrHits, len(hits)))
				endSpan(span, err)
				p.cfg.Observer.RecordRetrieval(ctx, ragtypes.RetrievalRecord{
					Retriever: name, Hits: len(hits), Duration: elapsed, Err: err,
				})
				stats[idx] = ragtypes.RetrievalStat{Retriever: name, QueryIndex: qi, Hits: len(hits), Duration: elapsed}
				if err != nil {
					stats[idx].Error = err.Error()
					retrieveErrs[idx] = fmt.Errorf("retriever %s query %d: %w", name, qi, err)
					if !errors.Is(err, ragtypes.ErrPartialSearch) {
						stats[idx].Hits = 0
						failed[idx] = true
						return
					}
				}
				allLists[idx] = ragtypes.RankedList{Retriever: name, QueryIndex: qi, Hits: hits}
			}()
		}
	}
	wg.Wait()

	var errs []error
	failures := 0
	succeeded := make([]ragtypes.RankedList, 0, totalPairs)
	for i, err := range retrieveErrs {
		if err != nil {
			errs = append(errs, err)
		}
		if failed[i] {
			failures++
			continue
		}
		succeeded = append(succeeded, allLists[i])
	}
	if failures == totalPairs {
		return nil, stats, nil, fmt.Errorf("retrieve: %w", errors.Join(errs...))
	}
	if len(errs) > 0 {
		partialErr = errors.Join(errs...)
	}
	return succeeded, stats, partialErr, nil
}

// fuse merges the ranked lists with the configured Fuser, applies optional
// recency blending, sorts by score, optionally collapses identical content,
// then applies the MinScore filter and keeps at most candidateK hits.
//
// MinScore is compared with the fused score divided by the highest score a
// hit could reach (ragtypes.FusionCeiling), so the threshold is in [0,1]
// whatever the number of retrievers and transformed queries. A fuser without
// a ceiling is normalized by the best fused score of the search.
func (p *pipelineImpl) fuse(lists []ragtypes.RankedList, cfg *ragtypes.SearchConfig, candidateK int) []ragtypes.SearchHit {
	fuseOpts := p.fuseOptions(cfg)
	merged := p.cfg.Fuser.Fuse(lists, fuseOpts)

	// Optional time-decay recency blending (opt-in via WithRecency).
	applyRecency(merged, cfg)

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].Variant.UUID < merged[j].Variant.UUID
	})

	if cfg.DedupContent {
		merged = dedupByContent(merged)
	}

	if cfg.MinScore > 0 && len(lists) > 0 && len(merged) > 0 {
		ceiling := merged[0].Score
		if fc, ok := p.cfg.Fuser.(ragtypes.FusionCeiling); ok {
			ceiling = fc.MaxFusedScore(lists, fuseOpts)
		}
		if ceiling > 0 {
			filtered := merged[:0]
			for _, hit := range merged {
				if hit.Score/ceiling >= cfg.MinScore {
					filtered = append(filtered, hit)
				}
			}
			merged = filtered
		}
	}

	if candidateK > 0 && len(merged) > candidateK {
		merged = merged[:candidateK]
	}
	return merged
}

// fuseOptions resolves a search's fusion settings: its own rank constant,
// else the pipeline's, and the pipeline's retriever weights overlaid with
// the search's.
func (p *pipelineImpl) fuseOptions(cfg *ragtypes.SearchConfig) ragtypes.FuseOptions {
	opts := ragtypes.FuseOptions{K: cfg.FusionK}
	if opts.K <= 0 {
		opts.K = p.cfg.FusionK
	}
	switch {
	case len(cfg.FusionWeights) == 0:
		opts.Weights = p.cfg.FusionWeights
	case len(p.cfg.FusionWeights) == 0:
		opts.Weights = cfg.FusionWeights
	default:
		opts.Weights = make(map[string]float64, len(p.cfg.FusionWeights)+len(cfg.FusionWeights))
		maps.Copy(opts.Weights, p.cfg.FusionWeights)
		maps.Copy(opts.Weights, cfg.FusionWeights)
	}
	return opts
}

// dedupByContent keeps the first hit of each fusion.ContentKey. hits must be
// sorted best first, so the kept hit is the highest ranked.
func dedupByContent(hits []ragtypes.SearchHit) []ragtypes.SearchHit {
	seen := make(map[string]bool, len(hits))
	out := hits[:0]
	for _, hit := range hits {
		key := fusion.ContentKey(&hit)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, hit)
	}
	return out
}

// expandNeighbors widens each text hit with up to n sections on each side,
// loading each document's sections once. Windows never overlap: a window
// stops before a section that a higher-ranked hit already covers, and a hit
// whose own section is covered is dropped. A document whose sections cannot
// be loaded keeps its hits unexpanded and contributes to the returned error.
func (p *pipelineImpl) expandNeighbors(ctx context.Context, hits []ragtypes.SearchHit, n int) ([]ragtypes.SearchHit, error) {
	sections := make(map[string][]ragtypes.Section)
	covered := make(map[string]map[int]bool)
	var errs []error
	out := make([]ragtypes.SearchHit, 0, len(hits))
	for _, hit := range hits {
		docUUID := hit.Provenance.DocumentUUID
		if hit.Variant.ContentType != ragtypes.ContentText || docUUID == "" {
			out = append(out, hit)
			continue
		}
		secs, ok := sections[docUUID]
		if !ok {
			loaded, err := p.cfg.Store.GetSections(ctx, docUUID)
			if err != nil {
				errs = append(errs, fmt.Errorf("neighbor sections of document %s: %w", docUUID, err))
				loaded = nil
			}
			secs = append([]ragtypes.Section(nil), loaded...)
			sort.SliceStable(secs, func(i, j int) bool { return secs[i].Index < secs[j].Index })
			sections[docUUID] = secs
			covered[docUUID] = make(map[int]bool)
		}
		pos := sectionPosition(secs, &hit.Provenance)
		if pos < 0 {
			out = append(out, hit)
			continue
		}
		seen := covered[docUUID]
		if seen[secs[pos].Index] {
			continue
		}
		lo, hi := max(0, pos-n), min(len(secs)-1, pos+n)
		for i := pos - 1; i >= lo; i-- {
			if seen[secs[i].Index] {
				lo = i + 1
				break
			}
		}
		for i := pos + 1; i <= hi; i++ {
			if seen[secs[i].Index] {
				hi = i - 1
				break
			}
		}
		var texts []string
		offset := 0
		for i := lo; i <= hi; i++ {
			seen[secs[i].Index] = true
			text := hit.Variant.Text
			if i != pos {
				text = sectionText(&secs[i])
			}
			if strings.TrimSpace(text) != "" {
				if i == pos {
					offset = len(strings.Join(texts, "\n\n"))
					if len(texts) > 0 {
						offset += len("\n\n")
					}
				}
				texts = append(texts, text)
			}
		}
		hit.Variant.Text = strings.Join(texts, "\n\n")
		hit.Highlight = hit.Highlight.Shifted(offset)
		hit.Provenance.Window = &ragtypes.SectionWindow{First: secs[lo].Index, Last: secs[hi].Index}
		out = append(out, hit)
	}
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// sectionPosition returns the position in secs of the section a hit came
// from, matching by UUID and then by index, or -1.
func sectionPosition(secs []ragtypes.Section, prov *ragtypes.Provenance) int {
	for i := range secs {
		if prov.SectionUUID != "" && secs[i].UUID == prov.SectionUUID {
			return i
		}
	}
	if prov.SectionUUID != "" {
		return -1
	}
	for i := range secs {
		if secs[i].Index == prov.SectionIndex {
			return i
		}
	}
	return -1
}

// sectionText joins the text of a section's variants, as parent expansion
// does.
func sectionText(sec *ragtypes.Section) string {
	var texts []string
	for _, v := range sec.Variants {
		if v.Text != "" {
			texts = append(texts, v.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}
