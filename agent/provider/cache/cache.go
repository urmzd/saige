// Package cache provides a response-caching decorator for types.Provider. It
// memoizes ChatStream responses keyed by a deterministic hash of
// (model, messages, tools, schema), mirroring the decorator pattern of
// agent/provider/retry and agent/provider/fallback. Only fully-completed,
// error-free streams are cached; cache hits replay recorded deltas and report a
// cache-hit UsageDelta that does not re-count tokens.
package cache

import (
	"context"
	"log/slog"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Config controls response caching.
type Config struct {
	// Cache stores recorded responses. Required.
	Cache types.Cache[CachedResponse]
	// TTL is passed to Cache.Set. Zero defers to the cache's default.
	TTL time.Duration
	// KeyNamespace is prefixed into every key so multiple providers/agents can
	// share one backing store without collisions (e.g. "anthropic").
	KeyNamespace string
	// Logger and Metrics default to noop.
	Logger  *slog.Logger
	Metrics types.Metrics
}

// Provider memoizes ChatStream responses. Only fully-completed, error-free
// streams are cached.
type Provider struct {
	inner types.Provider
	cfg   Config
}

var (
	_ types.Provider                 = (*Provider)(nil)
	_ types.StructuredOutputProvider = (*Provider)(nil)
	_ types.NamedProvider            = (*Provider)(nil)
)

// New wraps a provider with response caching. cfg.Cache is required.
func New(inner types.Provider, cfg Config) *Provider {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = types.NoopMetrics{}
	}
	return &Provider{inner: inner, cfg: cfg}
}

// Name implements types.NamedProvider.
func (p *Provider) Name() string {
	return "cache(" + types.ProviderName(p.inner) + ")"
}

// ChatStream implements types.Provider.
func (p *Provider) ChatStream(ctx context.Context, msgs []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return p.stream(ctx, msgs, tools, nil, func() (<-chan types.Delta, error) {
		return p.inner.ChatStream(ctx, msgs, tools)
	})
}

// ChatStreamWithSchema implements types.StructuredOutputProvider.
func (p *Provider) ChatStreamWithSchema(ctx context.Context, msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	call := func() (<-chan types.Delta, error) {
		if sp, ok := p.inner.(types.StructuredOutputProvider); ok {
			return sp.ChatStreamWithSchema(ctx, msgs, tools, schema)
		}
		return p.inner.ChatStream(ctx, msgs, tools) // schema lost, mirrors retry/fallback
	}
	return p.stream(ctx, msgs, tools, schema, call)
}

func (p *Provider) stream(
	ctx context.Context,
	msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema,
	call func() (<-chan types.Delta, error),
) (<-chan types.Delta, error) {
	if p.cfg.Cache == nil {
		return call() // no backing store: behave as a transparent passthrough
	}
	key := p.cfg.KeyNamespace + ":" + Key(types.ProviderModel(p.inner), msgs, tools, schema)

	// HIT: replay recorded deltas, no upstream call.
	if cr, found, err := p.cfg.Cache.Get(ctx, key); err == nil && found {
		p.cfg.Metrics.RecordProviderCall(ctx, "chat.cache_hit", p.Name(), 0, nil)
		return replay(cr), nil
	}

	// MISS: call upstream, tee into a recorder, persist only on clean completion.
	in, err := call()
	if err != nil {
		return nil, err // never cache provider construction errors
	}
	return p.recordAndTee(ctx, key, in), nil
}
