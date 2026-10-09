package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/knowledge"
	kgtool "github.com/urmzd/saige/rag/knowledge/tool"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/pgstore"
	ragtool "github.com/urmzd/saige/rag/tool"
	ragtypes "github.com/urmzd/saige/rag/types"

	googleProvider "github.com/urmzd/saige/agent/provider/google"
	ollamaProvider "github.com/urmzd/saige/agent/provider/ollama"
	openaiProvider "github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/rag/embedderregistry"
)

// connectPostgres creates a pgxpool.Pool from a DSN.
func connectPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
	if err != nil {
		return nil, err
	}
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return pool, nil
}

// textEmbedder adapts an Embed(ctx, []string) embedder to ragtypes.VariantEmbedder.
type textEmbedder struct {
	embed func(ctx context.Context, texts []string) ([][]float32, error)
}

func (e *textEmbedder) Embed(ctx context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	texts := make([]string, len(variants))
	for i, v := range variants {
		texts[i] = v.Text
	}
	return e.embed(ctx, texts)
}

// hostedEmbedder adapts a hosted provider's embed function: requests are
// split into bounded batches, and each batch is retried on transient
// failures such as a rate limit.
func hostedEmbedder(embed func(ctx context.Context, texts []string) ([][]float32, error)) ragtypes.VariantEmbedder {
	return embedderregistry.NewBatching(embedderregistry.NewRetrying(&textEmbedder{embed: embed}, retry.DefaultConfig()))
}

// resolveEmbedder creates a VariantEmbedder from the resolved embed provider
// flags. The embed provider follows --provider unless --embed-provider (or
// SAIGE_EMBED_PROVIDER) overrides it.
func resolveEmbedder(ctx context.Context, cf *commonFlags) (ragtypes.VariantEmbedder, kgtypes.Embedder, error) {
	name := cf.resolvedEmbedProvider()
	embedModel := cf.resolvedEmbedModel()

	switch name {
	case providerOllama:
		client := ollamaProvider.NewClient(*cf.ollamaHost, "", embedModel)
		emb := ollamaProvider.NewEmbedder(client)
		return embedderregistry.NewBatching(&textEmbedder{embed: emb.Embed}), emb, nil

	case providerOpenAI:
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, nil, fmt.Errorf("OPENAI_API_KEY is required")
		}
		// The retry decorator below owns retries, so the SDK makes one
		// attempt per call.
		opts := []openaiProvider.Option{openaiProvider.WithMaxRetries(0)}
		if *cf.baseURL != "" {
			opts = append(opts, openaiProvider.WithBaseURL(*cf.baseURL))
		}
		emb := openaiProvider.NewEmbedder(apiKey, embedModel, opts...)
		return hostedEmbedder(emb.Embed), emb, nil

	case providerGoogle:
		apiKey := os.Getenv("GOOGLE_API_KEY")
		if apiKey == "" {
			return nil, nil, fmt.Errorf("GOOGLE_API_KEY is required")
		}
		emb, err := googleProvider.NewEmbedder(ctx, apiKey, embedModel)
		if err != nil {
			return nil, nil, err
		}
		return hostedEmbedder(emb.Embed), emb, nil

	case providerAnthropic:
		return nil, nil, fmt.Errorf("anthropic does not provide an embedding API; set --embed-provider to openai, google, or ollama")

	default:
		return nil, nil, fmt.Errorf("unknown embedding provider %q; --embed-provider must be one of openai, google, ollama", name)
	}
}

// newRAGPipeline builds the RAG pipeline every CLI path shares. Ingest and
// search must embed with the same provider and model, or stored vectors will
// not match query vectors, so both go through this one constructor.
//
// The pipeline retrieves by vector similarity only. An in-memory BM25 index
// starts empty in every process and is filled only by Ingest in that same
// process, so in a CLI that runs one command per process it would never
// contain the documents a search is looking for.
//
// withEmbedder is false for commands that never embed (lookup, delete), so
// they work with any --provider, including one with no embedding API.
func newRAGPipeline(ctx context.Context, pool *pgxpool.Pool, cf *commonFlags, withEmbedder bool) (ragtypes.Pipeline, error) {
	var variantEmb ragtypes.VariantEmbedder
	if withEmbedder {
		var err error
		variantEmb, _, err = resolveEmbedder(ctx, cf)
		if err != nil {
			return nil, fmt.Errorf("rag embedder: %w", err)
		}
	}
	pipeline, err := rag.NewPipeline(ragPipelineOptions(pgstore.NewStore(pool, nil), variantEmb)...)
	if err != nil {
		return nil, fmt.Errorf("rag pipeline: %w", err)
	}
	return pipeline, nil
}

// ragPipelineOptions lists the pipeline options newRAGPipeline uses. A nil
// emb leaves the pipeline without embedders.
func ragPipelineOptions(store ragtypes.Store, emb ragtypes.VariantEmbedder) []rag.Option {
	opts := []rag.Option{
		rag.WithStore(store),
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithRecursiveChunker(512, 64),
	}
	if emb != nil {
		opts = append(opts, rag.WithEmbedders(newSingleEmbedderRegistry(emb)))
	}
	return opts
}

// newKnowledgeGraph builds the knowledge graph every CLI path shares, with an
// extractor (so ingest works) and the resolved embedder (so search has a
// vector leg and ingest stores comparable embeddings). withEmbedder is false
// for commands that never embed (graph, node).
func newKnowledgeGraph(ctx context.Context, pool *pgxpool.Pool, cf *commonFlags, withEmbedder bool) (kgtypes.Graph, error) {
	opts, err := knowledgeOptions(ctx, cf, withEmbedder)
	if err != nil {
		return nil, err
	}
	graph, err := knowledge.NewGraph(ctx, append(opts, knowledge.WithPostgres(pool))...)
	if err != nil {
		return nil, fmt.Errorf("kg graph: %w", err)
	}
	return graph, nil
}

// knowledgeOptions returns the extractor and embedder options for
// newKnowledgeGraph. The extractor uses the chat provider from --provider and
// resolves it lazily, so read-only commands need no LLM credentials. The
// embedder is resolved only when withEmbedder is set.
func knowledgeOptions(ctx context.Context, cf *commonFlags, withEmbedder bool) ([]knowledge.Option, error) {
	ext := newProviderExtractor(func() (agenttypes.Provider, error) {
		return resolveProvider(ctx, cf, false)
	})
	opts := []knowledge.Option{knowledge.WithExtractor(ext)}
	if withEmbedder {
		_, kgEmb, err := resolveEmbedder(ctx, cf)
		if err != nil {
			return nil, fmt.Errorf("kg embedder: %w", err)
		}
		opts = append(opts, knowledge.WithEmbedder(kgEmb))
	}
	return opts, nil
}

// buildTools connects RAG/KG databases and returns agent tools + cleanup function.
func buildTools(ctx context.Context, cf *commonFlags) ([]agenttypes.Tool, func(), error) {
	var tools []agenttypes.Tool
	var cleanups []func()

	cleanup := func() {
		for _, fn := range cleanups {
			fn()
		}
	}

	ragDSN := *cf.ragDB
	kgDSN := *cf.kgDB
	if ragDSN == "" && kgDSN == "" {
		return nil, func() {}, nil
	}

	// Shared pool if DSNs match.
	pools := map[string]*pgxpool.Pool{}
	getPool := func(dsn string) (*pgxpool.Pool, error) {
		if p, ok := pools[dsn]; ok {
			return p, nil
		}
		p, err := connectPostgres(ctx, dsn)
		if err != nil {
			return nil, err
		}
		pools[dsn] = p
		cleanups = append(cleanups, p.Close)
		return p, nil
	}

	if ragDSN != "" {
		pool, err := getPool(ragDSN)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rag db: %w", err)
		}

		pipeline, err := newRAGPipeline(ctx, pool, cf, true)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		cleanups = append(cleanups, func() { _ = pipeline.Close(ctx) })
		tools = append(tools, ragtool.NewTools(pipeline)...)
	}

	if kgDSN != "" {
		pool, err := getPool(kgDSN)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("kg db: %w", err)
		}

		graph, err := newKnowledgeGraph(ctx, pool, cf, true)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		cleanups = append(cleanups, func() { _ = graph.Close(ctx) })
		tools = append(tools, kgtool.NewTools(graph)...)
	}

	return tools, cleanup, nil
}

// singleEmbedderRegistry is a minimal EmbedderRegistry that uses one embedder for all types.
type singleEmbedderRegistry struct {
	embedder ragtypes.VariantEmbedder
}

func newSingleEmbedderRegistry(e ragtypes.VariantEmbedder) *singleEmbedderRegistry {
	return &singleEmbedderRegistry{embedder: e}
}

func (r *singleEmbedderRegistry) Register(_ ragtypes.ContentType, _ ragtypes.VariantEmbedder) {}

func (r *singleEmbedderRegistry) Embed(ctx context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	return r.embedder.Embed(ctx, variants)
}
