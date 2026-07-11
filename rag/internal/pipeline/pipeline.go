// Package pipeline implements the RAG Pipeline interface.
//
// # Write atomicity
//
// The RAG store write is atomic per store implementation (pgstore wraps the
// document/section/variant inserts in one transaction; memstore applies them
// under a single lock). Knowledge graph enrichment, however, targets a
// separate store and CANNOT be made atomic with the RAG write: the pipeline
// therefore commits the RAG write first and runs graph enrichment afterwards.
// A graph-stage failure never destroys the committed document; it surfaces as
// an error wrapping ragtypes.ErrPartialIngest alongside the (valid) ingest
// result. This means a document can exist in the RAG store without its
// derived graph facts (or, on delete, graph facts can briefly outlive the
// document if the graph deletion fails); callers needing strict consistency
// must reconcile the two stores themselves.
package pipeline

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag/contextassembler"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// defaultRRFK is the default Reciprocal Rank Fusion constant, overridable per
// search via ragtypes.WithFusionK.
const defaultRRFK = 60

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

// Config holds the pipeline's dependencies.
type Config struct {
	Store            ragtypes.Store
	ContentExtractor ragtypes.ContentExtractor
	Chunker          ragtypes.Chunker
	Embedders        ragtypes.EmbedderRegistry
	Graph            knowledgetypes.Graph
	DedupBehavior    ragtypes.DedupBehavior
	StoreOriginals   bool
	Logger           *slog.Logger
	QueryTransformer ragtypes.QueryTransformer
	Retrievers       []ragtypes.Retriever
	Reranker         ragtypes.Reranker
	ContextAssembler ragtypes.ContextAssembler
}

type pipelineImpl struct {
	cfg Config
}

// New creates a new pipeline with the given configuration.
func New(cfg Config) ragtypes.Pipeline {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &pipelineImpl{cfg: cfg}
}

func (p *pipelineImpl) Ingest(ctx context.Context, raw *ragtypes.RawDocument) (*ragtypes.IngestResult, error) {
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw.Data))

	existing, err := p.cfg.Store.FindByFingerprint(ctx, fingerprint)
	if err != nil && err != ragtypes.ErrDocumentNotFound {
		return nil, fmt.Errorf("fingerprint lookup: %w", err)
	}
	if existing != nil && p.cfg.DedupBehavior == ragtypes.DedupSkip {
		return &ragtypes.IngestResult{
			DocumentUUID: existing.UUID,
			Deduplicated: true,
		}, nil
	}
	// DedupReplace: the existing document is NOT deleted here. All fallible
	// preparation (extract, chunk, embed) runs first; the replace happens as a
	// single store-level operation below so a mid-stage failure leaves the
	// prior document intact.

	doc, err := p.cfg.ContentExtractor.Extract(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("extract content: %w", err)
	}
	doc.Fingerprint = fingerprint

	if p.cfg.Chunker != nil {
		doc, err = p.cfg.Chunker.Chunk(ctx, doc)
		if err != nil {
			return nil, fmt.Errorf("chunk: %w", err)
		}
	}

	if p.cfg.Embedders != nil {
		var allVariants []ragtypes.ContentVariant
		for _, sec := range doc.Sections {
			allVariants = append(allVariants, sec.Variants...)
		}
		if len(allVariants) > 0 {
			embeddings, err := p.cfg.Embedders.Embed(ctx, allVariants)
			if err != nil {
				return nil, fmt.Errorf("embed variants: %w", err)
			}
			idx := 0
			for i := range doc.Sections {
				for j := range doc.Sections[i].Variants {
					doc.Sections[i].Variants[j].Embedding = embeddings[idx]
					idx++
				}
			}
		}
	}

	if existing != nil && p.cfg.DedupBehavior == ragtypes.DedupReplace {
		if replacer, ok := p.cfg.Store.(ragtypes.DocumentReplacer); ok {
			if err := replacer.ReplaceDocument(ctx, existing.UUID, doc); err != nil {
				return nil, fmt.Errorf("replace document: %w", err)
			}
		} else {
			// Fallback for stores without atomic replace: by this point all
			// fallible preparation has succeeded, so the unprotected window is
			// limited to the store write itself.
			if err := p.cfg.Store.DeleteDocument(ctx, existing.UUID); err != nil {
				return nil, fmt.Errorf("delete existing document: %w", err)
			}
			if err := p.cfg.Store.CreateDocument(ctx, doc); err != nil {
				return nil, fmt.Errorf("create document: %w", err)
			}
		}
		// The replaced document's graph episodes are stale; remove them if the
		// graph supports deletion.
		p.deleteGraphEpisodes(ctx, existing.UUID)
	} else if err := p.cfg.Store.CreateDocument(ctx, doc); err != nil {
		return nil, fmt.Errorf("create document: %w", err)
	}

	if p.cfg.StoreOriginals {
		if err := p.cfg.Store.StoreOriginal(ctx, doc.UUID, raw.Data); err != nil {
			return nil, fmt.Errorf("store original: %w", err)
		}
	}

	// Call Indexer.Index on any retrievers that implement Indexer.
	for _, retriever := range p.cfg.Retrievers {
		if indexer, ok := retriever.(ragtypes.Indexer); ok {
			if err := indexer.Index(ctx, doc); err != nil {
				return nil, fmt.Errorf("index for retriever: %w", err)
			}
		}
	}

	// Graph enrichment runs against a separate store after the RAG write has
	// committed (see package doc). Failures surface as ErrPartialIngest
	// alongside the valid result rather than destroying the committed document.
	var graphErrs []error
	if p.cfg.Graph != nil {
		for _, sec := range doc.Sections {
			for _, v := range sec.Variants {
				if v.ContentType != ragtypes.ContentText || v.Text == "" {
					continue
				}
				name := sec.Heading
				if name == "" {
					name = fmt.Sprintf("section-%d", sec.Index)
				}
				_, err := p.cfg.Graph.IngestEpisode(ctx, &knowledgetypes.EpisodeInput{
					Name:    name,
					Body:    v.Text,
					Source:  doc.SourceURI,
					GroupID: doc.UUID,
					Metadata: map[string]string{
						"content_type": string(v.ContentType),
						"section_uuid": sec.UUID,
						"variant_uuid": v.UUID,
					},
				})
				if err != nil {
					p.cfg.Logger.WarnContext(ctx, "kg ingest failed",
						"section", sec.UUID, "error", err)
					graphErrs = append(graphErrs, fmt.Errorf("kg ingest section %s: %w", sec.UUID, err))
				}
			}
		}
	}

	variantCount := 0
	for _, sec := range doc.Sections {
		variantCount += len(sec.Variants)
	}

	result := &ragtypes.IngestResult{
		DocumentUUID: doc.UUID,
		Sections:     len(doc.Sections),
		Variants:     variantCount,
	}
	if len(graphErrs) > 0 {
		return result, fmt.Errorf("%w: %w", ragtypes.ErrPartialIngest, errors.Join(graphErrs...))
	}
	return result, nil
}

// deleteGraphEpisodes removes graph episodes grouped under documentUUID when
// the configured graph supports deletion. Failures (and graphs without a
// deletion API) are logged, not fatal: the graph is a derived, best-effort
// store (see package doc).
func (p *pipelineImpl) deleteGraphEpisodes(ctx context.Context, documentUUID string) {
	if p.cfg.Graph == nil {
		return
	}
	deleter, ok := p.cfg.Graph.(ragtypes.GraphEpisodeDeleter)
	if !ok {
		p.cfg.Logger.WarnContext(ctx, "graph does not support episode deletion; derived facts retained",
			"document", documentUUID)
		return
	}
	if err := deleter.DeleteEpisodes(ctx, documentUUID); err != nil {
		p.cfg.Logger.WarnContext(ctx, "graph episode delete failed",
			"document", documentUUID, "error", err)
	}
}

func (p *pipelineImpl) Search(ctx context.Context, query string, opts ...ragtypes.SearchOption) (*ragtypes.SearchPipelineResult, error) {
	cfg := &ragtypes.SearchConfig{Limit: 10}
	for _, o := range opts {
		o(cfg)
	}

	if len(p.cfg.Retrievers) == 0 {
		return nil, fmt.Errorf("%w", ragtypes.ErrNoRetriever)
	}

	// Step 1: Query transformation.
	queries := []string{query}
	if p.cfg.QueryTransformer != nil {
		transformed, err := p.cfg.QueryTransformer.Transform(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("transform query: %w", err)
		}
		if len(transformed) > 0 {
			queries = transformed
		}
	}

	// Step 2: Retrieve from all retrievers for all queries.
	searchOpts := &ragtypes.SearchOptions{
		ContentTypes:    cfg.ContentTypes,
		Limit:           cfg.Limit,
		MetadataFilters: cfg.MetadataFilters,
	}

	// Collect per-retriever ranked lists for RRF (parallel). Retriever
	// failures are tolerated as long as at least one retrieval succeeds: the
	// search continues with the successful lists and the failures are surfaced
	// as an error wrapping ragtypes.ErrPartialSearch alongside the result.
	// Only when ALL retrievals fail does Search return a nil result.
	type rankedList struct {
		hits []ragtypes.SearchHit
	}
	totalPairs := len(p.cfg.Retrievers) * len(queries)
	allLists := make([]rankedList, totalPairs)
	retrieveErrs := make([]error, totalPairs)

	var wg sync.WaitGroup
	for ri, retriever := range p.cfg.Retrievers {
		for qi, q := range queries {
			idx := ri*len(queries) + qi
			wg.Add(1)
			go func() {
				defer wg.Done()
				hits, err := retriever.Retrieve(ctx, q, searchOpts)
				if err != nil {
					retrieveErrs[idx] = fmt.Errorf("retriever %d query %q: %w", ri, q, err)
					return
				}
				allLists[idx] = rankedList{hits: hits}
			}()
		}
	}
	wg.Wait()

	var errs []error
	for _, err := range retrieveErrs {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == totalPairs {
		return nil, fmt.Errorf("retrieve: %w", errors.Join(errs...))
	}
	var partialErr error
	if len(errs) > 0 {
		partialErr = fmt.Errorf("%w: %w", ragtypes.ErrPartialSearch, errors.Join(errs...))
	}

	// Step 3: RRF merge + dedup by variant UUID.
	rrfK := cfg.FusionK
	if rrfK <= 0 {
		rrfK = defaultRRFK
	}
	scores := make(map[string]float64)            // variant UUID -> RRF score
	hitMap := make(map[string]ragtypes.SearchHit) // variant UUID -> best hit

	for _, list := range allLists {
		for rank, hit := range list.hits {
			uuid := hit.Variant.UUID
			rrfScore := 1.0 / float64(rrfK+rank+1)
			scores[uuid] += rrfScore
			if existing, ok := hitMap[uuid]; !ok || hit.Score > existing.Score {
				hitMap[uuid] = hit
			}
		}
	}

	// Build merged results.
	merged := make([]ragtypes.SearchHit, 0, len(hitMap))
	for uuid, hit := range hitMap {
		hit.Score = scores[uuid]
		merged = append(merged, hit)
	}

	// Step 3.5: Optional time-decay recency blending (opt-in via WithRecency).
	applyRecency(merged, cfg)

	// Sort by (recency-adjusted) score descending.
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Score > merged[j].Score
	})

	// Step 4: MinScore filter (after fusion).
	if cfg.MinScore > 0 {
		filtered := merged[:0]
		for _, hit := range merged {
			if hit.Score >= cfg.MinScore {
				filtered = append(filtered, hit)
			}
		}
		merged = filtered
	}

	// Step 5: Limit.
	if cfg.Limit > 0 && len(merged) > cfg.Limit {
		merged = merged[:cfg.Limit]
	}

	// Step 6: Rerank.
	if p.cfg.Reranker != nil && len(merged) > 0 {
		reranked, err := p.cfg.Reranker.Rerank(ctx, query, merged)
		if err != nil {
			return nil, fmt.Errorf("rerank: %w", err)
		}
		merged = reranked
	}

	// Step 7: Context assembly.
	result := &ragtypes.SearchPipelineResult{
		Query: query,
		Hits:  merged,
	}
	if len(queries) > 1 {
		result.TransformedQueries = queries
	}

	if cfg.AssembleContext && len(merged) > 0 {
		assembler := p.cfg.ContextAssembler
		if assembler == nil {
			assembler = &contextassembler.DefaultAssembler{MaxTokens: cfg.MaxTokens}
		}
		assembled, err := assembler.Assemble(ctx, query, merged)
		if err != nil {
			return nil, fmt.Errorf("assemble context: %w", err)
		}
		result.Context = assembled
	}

	return result, partialErr
}

func (p *pipelineImpl) Lookup(ctx context.Context, variantUUID string) (*ragtypes.SearchHit, error) {
	variant, prov, err := p.cfg.Store.GetVariant(ctx, variantUUID)
	if err != nil {
		return nil, fmt.Errorf("get variant: %w", err)
	}
	return &ragtypes.SearchHit{
		Variant:    *variant,
		Score:      1.0,
		Provenance: *prov,
	}, nil
}

func (p *pipelineImpl) Update(ctx context.Context, documentUUID string, raw *ragtypes.RawDocument) (*ragtypes.IngestResult, error) {
	if err := p.Delete(ctx, documentUUID); err != nil && err != ragtypes.ErrDocumentNotFound {
		return nil, fmt.Errorf("delete old document: %w", err)
	}
	return p.Ingest(ctx, raw)
}

func (p *pipelineImpl) Delete(ctx context.Context, documentUUID string) error {
	// Call Indexer.Remove on any retrievers that implement Indexer.
	for _, retriever := range p.cfg.Retrievers {
		if indexer, ok := retriever.(ragtypes.Indexer); ok {
			if err := indexer.Remove(ctx, documentUUID); err != nil {
				p.cfg.Logger.WarnContext(ctx, "retriever index remove failed", "error", err)
			}
		}
	}

	// Remove graph episodes derived from this document (grouped by its UUID).
	p.deleteGraphEpisodes(ctx, documentUUID)

	return p.cfg.Store.DeleteDocument(ctx, documentUUID)
}

func (p *pipelineImpl) Reconstruct(ctx context.Context, documentUUID string) (*ragtypes.Document, error) {
	doc, err := p.cfg.Store.GetDocument(ctx, documentUUID)
	if err != nil {
		return nil, fmt.Errorf("get document: %w", err)
	}
	return doc, nil
}

func (p *pipelineImpl) Close(ctx context.Context) error {
	return p.cfg.Store.Close(ctx)
}
