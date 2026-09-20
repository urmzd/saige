package knowledge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/rag/knowledge/internal/engine"
	"github.com/urmzd/saige/rag/knowledge/pgstore"
	"github.com/urmzd/saige/rag/knowledge/types"
)

// Config holds configuration for creating a Graph.
type Config struct {
	PostgresPool *pgxpool.Pool
	Extractor    types.Extractor
	Embedder     types.Embedder
	Logger       *slog.Logger
	Store        types.Store
}

// Option configures kg.
type Option func(*Config)

// WithPostgres configures a PostgreSQL backend using a shared connection pool.
func WithPostgres(pool *pgxpool.Pool) Option {
	return func(c *Config) {
		c.PostgresPool = pool
	}
}

// WithExtractor sets the entity/relation extractor.
func WithExtractor(ext types.Extractor) Option {
	return func(c *Config) {
		c.Extractor = ext
	}
}

// WithEmbedder sets the vector embedder.
func WithEmbedder(emb types.Embedder) Option {
	return func(c *Config) {
		c.Embedder = emb
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Config) {
		c.Logger = logger
	}
}

// WithStore sets a pre-created store, skipping automatic store creation.
// Use this when you need direct access to the store (e.g. for DB connection sharing).
func WithStore(s types.Store) Option {
	return func(c *Config) {
		c.Store = s
	}
}

// NewGraph creates a new Graph using the provided options.
// This wires up the GraphEngine with the configured Store, Extractor, and Embedder.
func NewGraph(ctx context.Context, opts ...Option) (types.Graph, error) {
	cfg := &Config{}
	for _, o := range opts {
		o(cfg)
	}

	var store types.Store
	if cfg.Store != nil {
		store = cfg.Store
	} else if cfg.PostgresPool != nil {
		store = pgstore.NewStore(cfg.PostgresPool, cfg.Logger)
	} else {
		return nil, fmt.Errorf("no backend configured: use WithPostgres or WithStore")
	}

	engineOpts := []engine.Option{
		engine.WithStore(store),
	}
	if cfg.Extractor != nil {
		engineOpts = append(engineOpts, engine.WithExtractor(cfg.Extractor))
	}
	if cfg.Embedder != nil {
		engineOpts = append(engineOpts, engine.WithEmbedder(cfg.Embedder))
	}
	if cfg.Logger != nil {
		engineOpts = append(engineOpts, engine.WithLogger(cfg.Logger))
	}

	return engine.New(engineOpts...), nil
}
