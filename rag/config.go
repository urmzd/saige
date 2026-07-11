// Package rag provides a multi-modal RAG development kit with search pipeline.
package rag

import (
	"fmt"
	"log/slog"

	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/chunker"
	"github.com/urmzd/saige/rag/contextassembler"
	"github.com/urmzd/saige/rag/graphretriever"
	"github.com/urmzd/saige/rag/hyde"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/parentretriever"
	"github.com/urmzd/saige/rag/reranker"
	ragtypes "github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

// Config holds configuration for creating a Pipeline.
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

	// Internal flags for convenience options.
	bm25Config        *bm25retriever.Config
	recursiveChunker  *chunker.Config
	semanticChunker   *chunker.SemanticConfig
	hydeConfig        *hydeConfig
	mmrLambda         float64
	crossEncoderScore reranker.Scorer
	parentContext     bool
	compressionLLM    ragtypes.LLM
}

type hydeConfig struct {
	llm             ragtypes.LLM
	numHypothetical int
}

// Option configures a Pipeline.
type Option func(*Config)

// WithStore sets the document store.
func WithStore(s ragtypes.Store) Option {
	return func(c *Config) { c.Store = s }
}

// WithContentExtractor sets the content extractor.
func WithContentExtractor(ext ragtypes.ContentExtractor) Option {
	return func(c *Config) { c.ContentExtractor = ext }
}

// WithChunker sets the chunker.
func WithChunker(ch ragtypes.Chunker) Option {
	return func(c *Config) { c.Chunker = ch }
}

// WithEmbedders sets the embedder registry.
func WithEmbedders(reg ragtypes.EmbedderRegistry) Option {
	return func(c *Config) { c.Embedders = reg }
}

// WithGraph sets the knowledge graph. Ingested documents are enriched into
// the graph, a graph retriever is registered into the search fusion set, and
// document deletes remove the graph episodes derived from the document (when
// the graph implements ragtypes.GraphEpisodeDeleter). Do not also pass a
// graphretriever via WithRetrievers, or graph results will be fused twice.
func WithGraph(g knowledgetypes.Graph) Option {
	return func(c *Config) { c.Graph = g }
}

// WithDedupBehavior sets the deduplication behavior.
func WithDedupBehavior(b ragtypes.DedupBehavior) Option {
	return func(c *Config) { c.DedupBehavior = b }
}

// WithStoreOriginals enables storing original document bytes.
func WithStoreOriginals(b bool) Option {
	return func(c *Config) { c.StoreOriginals = b }
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Config) { c.Logger = logger }
}

// WithQueryTransformer sets the query transformer for multi-query expansion.
func WithQueryTransformer(qt ragtypes.QueryTransformer) Option {
	return func(c *Config) { c.QueryTransformer = qt }
}

// WithRetrievers sets explicit retrievers for the search pipeline.
func WithRetrievers(retrievers ...ragtypes.Retriever) Option {
	return func(c *Config) { c.Retrievers = retrievers }
}

// WithReranker sets the reranker for search result reordering.
func WithReranker(r ragtypes.Reranker) Option {
	return func(c *Config) { c.Reranker = r }
}

// WithContextAssembler sets the context assembler for citation generation.
func WithContextAssembler(a ragtypes.ContextAssembler) Option {
	return func(c *Config) { c.ContextAssembler = a }
}

// WithBM25 adds a BM25 lexical retriever to the pipeline. If cfg is nil, defaults are used.
func WithBM25(cfg *bm25retriever.Config) Option {
	return func(c *Config) {
		if cfg == nil {
			cfg = bm25retriever.DefaultConfig()
		}
		c.bm25Config = cfg
	}
}

// WithRecursiveChunker sets a recursive text chunker with the given token limits.
func WithRecursiveChunker(maxTokens, overlap int) Option {
	return func(c *Config) {
		c.recursiveChunker = &chunker.Config{
			MaxTokens:  maxTokens,
			Overlap:    overlap,
			Separators: []string{"\n\n", "\n", ". ", " "},
		}
	}
}

// WithSemanticChunker sets a semantic chunker that splits by embedding similarity.
// Requires Embedders to be set.
func WithSemanticChunker(threshold float64, minTokens, maxTokens int) Option {
	return func(c *Config) {
		c.semanticChunker = &chunker.SemanticConfig{
			Threshold: threshold,
			MinTokens: minTokens,
			MaxTokens: maxTokens,
		}
	}
}

// WithHyDE sets a HyDE query transformer that generates hypothetical documents via LLM.
func WithHyDE(llm ragtypes.LLM, numHypothetical int) Option {
	return func(c *Config) {
		c.hydeConfig = &hydeConfig{llm: llm, numHypothetical: numHypothetical}
	}
}

// WithMMR sets an MMR diversity reranker. Lambda controls relevance vs diversity (higher = more relevance).
func WithMMR(lambda float64) Option {
	return func(c *Config) { c.mmrLambda = lambda }
}

// WithCrossEncoder sets a cross-encoder reranker with the given scorer.
func WithCrossEncoder(scorer reranker.Scorer) Option {
	return func(c *Config) { c.crossEncoderScore = scorer }
}

// WithParentContext wraps each retriever to expand hits with full parent section text.
func WithParentContext() Option {
	return func(c *Config) { c.parentContext = true }
}

// WithCompression sets a compressing context assembler that uses an LLM to extract relevant text.
func WithCompression(llm ragtypes.LLM) Option {
	return func(c *Config) { c.compressionLLM = llm }
}

// NewPipeline creates a new Pipeline using the provided options.
func NewPipeline(opts ...Option) (ragtypes.Pipeline, error) {
	cfg := &Config{}
	for _, o := range opts {
		o(cfg)
	}

	if cfg.Store == nil {
		return nil, fmt.Errorf("%w", ragtypes.ErrNoStore)
	}
	if cfg.ContentExtractor == nil {
		return nil, fmt.Errorf("%w", ragtypes.ErrNoExtractor)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	// Auto-wire chunker from convenience options.
	if cfg.Chunker == nil {
		if cfg.semanticChunker != nil && cfg.Embedders != nil {
			cfg.Chunker = chunker.NewSemantic(cfg.Embedders, cfg.semanticChunker)
		} else if cfg.recursiveChunker != nil {
			cfg.Chunker = chunker.NewRecursive(cfg.recursiveChunker)
		}
	}

	// Auto-wire query transformer from convenience options.
	if cfg.QueryTransformer == nil && cfg.hydeConfig != nil {
		cfg.QueryTransformer = hyde.New(hyde.Config{
			LLM:             cfg.hydeConfig.llm,
			NumHypothetical: cfg.hydeConfig.numHypothetical,
		})
	}

	// Auto-wire reranker from convenience options.
	if cfg.Reranker == nil {
		if cfg.crossEncoderScore != nil {
			cfg.Reranker = reranker.NewCrossEncoder(cfg.crossEncoderScore)
		} else if cfg.mmrLambda > 0 {
			cfg.Reranker = reranker.NewMMR(cfg.mmrLambda)
		}
	}

	// Auto-wire context assembler from convenience options.
	if cfg.ContextAssembler == nil && cfg.compressionLLM != nil {
		cfg.ContextAssembler = contextassembler.NewCompressing(cfg.compressionLLM, 0)
	}

	// Build retriever list.
	retrievers := cfg.Retrievers

	// Auto-create a VectorRetriever if no explicit retrievers but embedders are set.
	if len(retrievers) == 0 && cfg.Embedders != nil {
		retrievers = []ragtypes.Retriever{vectorretriever.New(cfg.Store, cfg.Embedders)}
	}

	// Add BM25 retriever if configured.
	if cfg.bm25Config != nil {
		retrievers = append(retrievers, bm25retriever.New(cfg.Store, cfg.bm25Config))
	}

	// Add a graph retriever so WithGraph contributes to search fusion, not
	// just ingest-time enrichment.
	if cfg.Graph != nil {
		retrievers = append(retrievers, graphretriever.New(cfg.Graph, cfg.Store))
	}

	// Wrap retrievers with parent context if enabled.
	if cfg.parentContext {
		wrapped := make([]ragtypes.Retriever, len(retrievers))
		for i, r := range retrievers {
			wrapped[i] = parentretriever.New(r, cfg.Store)
		}
		retrievers = wrapped
	}

	return pipeline.New(pipeline.Config{
		Store:            cfg.Store,
		ContentExtractor: cfg.ContentExtractor,
		Chunker:          cfg.Chunker,
		Embedders:        cfg.Embedders,
		Graph:            cfg.Graph,
		DedupBehavior:    cfg.DedupBehavior,
		StoreOriginals:   cfg.StoreOriginals,
		Logger:           cfg.Logger,
		QueryTransformer: cfg.QueryTransformer,
		Retrievers:       retrievers,
		Reranker:         cfg.Reranker,
		ContextAssembler: cfg.ContextAssembler,
	}), nil
}
