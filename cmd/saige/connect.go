package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/knowledge"
	kgtool "github.com/urmzd/saige/knowledge/tool"
	kgtypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/pgstore"
	ragtool "github.com/urmzd/saige/rag/tool"
	ragtypes "github.com/urmzd/saige/rag/types"

	ollamaProvider "github.com/urmzd/saige/agent/provider/ollama"
	openaiProvider "github.com/urmzd/saige/agent/provider/openai"
	googleProvider "github.com/urmzd/saige/agent/provider/google"
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

// resolveEmbedder creates a VariantEmbedder from the resolved provider flags.
func resolveEmbedder(ctx context.Context, cf *commonFlags) (ragtypes.VariantEmbedder, kgtypes.Embedder, error) {
	name := cf.resolvedProvider()
	embedModel := cf.resolvedEmbedModel()

	switch name {
	case providerOllama:
		client := ollamaProvider.NewClient(*cf.ollamaHost, "", embedModel)
		emb := ollamaProvider.NewEmbedder(client)
		return &textEmbedder{embed: emb.Embed}, emb, nil

	case providerOpenAI:
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, nil, fmt.Errorf("OPENAI_API_KEY is required")
		}
		var opts []openaiProvider.Option
		if *cf.baseURL != "" {
			opts = append(opts, openaiProvider.WithBaseURL(*cf.baseURL))
		}
		emb := openaiProvider.NewEmbedder(apiKey, embedModel, opts...)
		return &textEmbedder{embed: emb.Embed}, emb, nil

	case providerGoogle:
		apiKey := os.Getenv("GOOGLE_API_KEY")
		if apiKey == "" {
			return nil, nil, fmt.Errorf("GOOGLE_API_KEY is required")
		}
		emb, err := googleProvider.NewEmbedder(ctx, apiKey, embedModel)
		if err != nil {
			return nil, nil, err
		}
		return &textEmbedder{embed: emb.Embed}, emb, nil

	case providerAnthropic:
		return nil, nil, fmt.Errorf("anthropic does not provide an embedding API; use --embed-model with another provider or use ollama")

	default:
		return nil, nil, fmt.Errorf("unknown provider for embeddings: %s", name)
	}
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

		variantEmb, _, err := resolveEmbedder(ctx, cf)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rag embedder: %w", err)
		}

		store := pgstore.NewStore(pool, nil)
		pipeline, err := rag.NewPipeline(
			rag.WithStore(store),
			rag.WithContentExtractor(extractor.NewAuto()),
			rag.WithRecursiveChunker(512, 64),
			rag.WithEmbedders(newSingleEmbedderRegistry(variantEmb)),
			rag.WithBM25(nil),
		)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rag pipeline: %w", err)
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

		_, kgEmb, err := resolveEmbedder(ctx, cf)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("kg embedder: %w", err)
		}

		graph, err := knowledge.NewGraph(ctx,
			knowledge.WithPostgres(pool),
			knowledge.WithEmbedder(kgEmb),
		)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("kg graph: %w", err)
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
