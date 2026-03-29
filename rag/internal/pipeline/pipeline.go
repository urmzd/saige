// Package pipeline implements the RAG Pipeline interface.
package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"

	"golang.org/x/sync/errgroup"

	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag/contextassembler"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// Config holds the pipeline's dependencies.
type Config struct {
	Store            ragtypes.Store
	ContentExtractor ragtypes.ContentExtractor
	Chunker          ragtypes.Chunker
	Embedders        ragtypes.EmbedderRegistry
	Graph          knowledgetypes.Graph
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
	return &pipelineImpl{cfg: cfg}
}

func (p *pipelineImpl) Ingest(ctx context.Context, raw *ragtypes.RawDocument) (*ragtypes.IngestResult, error) {
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw.Data))

	existing, err := p.cfg.Store.FindByFingerprint(ctx, fingerprint)
	if err != nil && err != ragtypes.ErrDocumentNotFound {
		return nil, fmt.Errorf("fingerprint lookup: %w", err)
	}
	if existing != nil {
		switch p.cfg.DedupBehavior {
		case ragtypes.DedupSkip:
			return &ragtypes.IngestResult{
				DocumentUUID: existing.UUID,
				Deduplicated: true,
			}, nil
		case ragtypes.DedupReplace:
			if err := p.cfg.Store.DeleteDocument(ctx, existing.UUID); err != nil {
				return nil, fmt.Errorf("delete existing document: %w", err)
			}
		}
	}

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

	if err := p.cfg.Store.CreateDocument(ctx, doc); err != nil {
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
				}
			}
		}
	}

	variantCount := 0
	for _, sec := range doc.Sections {
		variantCount += len(sec.Variants)
	}

	return &ragtypes.IngestResult{
		DocumentUUID: doc.UUID,
		Sections:     len(doc.Sections),
		Variants:     variantCount,
	}, nil
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

	// Collect per-retriever ranked lists for RRF (parallel).
	type rankedList struct {
		hits []ragtypes.SearchHit
	}
	totalPairs := len(p.cfg.Retrievers) * len(queries)
	allLists := make([]rankedList, totalPairs)

	g, gctx := errgroup.WithContext(ctx)
	for ri, retriever := range p.cfg.Retrievers {
		for qi, q := range queries {
			idx := ri*len(queries) + qi
			g.Go(func() error {
				hits, err := retriever.Retrieve(gctx, q, searchOpts)
				if err != nil {
					return err
				}
				allLists[idx] = rankedList{hits: hits}
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("retrieve: %w", err)
	}

	// Step 3: RRF merge + dedup by variant UUID.
	const rrfK = 60
	scores := make(map[string]float64)    // variant UUID -> RRF score
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

	// Sort by RRF score descending.
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

	return result, nil
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
