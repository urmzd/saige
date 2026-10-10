package knowledge

import (
	"fmt"
	"log/slog"

	agenttypes "github.com/urmzd/saige/agent/types"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/rag/knowledge/internal/engine"
	"github.com/urmzd/saige/rag/knowledge/pgstore"
	"github.com/urmzd/saige/rag/knowledge/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// Config holds configuration for creating a Graph.
type Config struct {
	PostgresPool *pgxpool.Pool
	Extractor    types.Extractor
	Embedder     types.Embedder
	Logger       *slog.Logger
	Store        types.Store
	// StrictOntology drops extracted entities and relations whose type is
	// not in the applied ontology.
	StrictOntology bool
	// Observer receives spans and metrics. See WithObserver.
	Observer ragtypes.Observer
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

// WithStrictOntology drops extracted entities and relations whose type is
// not in the ontology passed to ApplyOntology. Without it, types that match
// an ontology type ignoring case and punctuation are rewritten to the
// ontology's spelling and other types are kept as extracted.
func WithStrictOntology() Option {
	return func(c *Config) {
		c.StrictOntology = true
	}
}

// WithObserver reports episode ingest, fact search, and deletion to
// observer as spans, and each fact search as a retrieval metric. See
// Observe. The rag/otel package adapts OpenTelemetry to ragtypes.Observer.
func WithObserver(observer ragtypes.Observer) Option {
	return func(c *Config) {
		c.Observer = observer
	}
}

// WithStore sets a pre-created store, skipping automatic store creation.
// Use this when you need direct access to the store (e.g. for DB connection sharing).
func WithStore(s types.Store) Option {
	return func(c *Config) {
		c.Store = s
	}
}

// New creates a Graph from cfg and opts, which apply on top of it. This
// wires up the GraphEngine with the configured Store, Extractor, and
// Embedder. A configuration with neither a store nor a pool is an error
// wrapping agent/types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (types.Graph, error) {
	for _, o := range opts {
		o(&cfg)
	}

	var store types.Store
	if cfg.Store != nil {
		store = cfg.Store
	} else if cfg.PostgresPool != nil {
		pg, err := pgstore.New(pgstore.Config{Pool: cfg.PostgresPool, Logger: cfg.Logger})
		if err != nil {
			return nil, err
		}
		store = pg
	} else {
		return nil, fmt.Errorf("%w: knowledge: no backend configured: use WithPostgres or WithStore", agenttypes.ErrInvalidConfig)
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
	if cfg.StrictOntology {
		engineOpts = append(engineOpts, engine.WithStrictOntology())
	}

	return Observe(engine.New(engineOpts...), cfg.Observer), nil
}
