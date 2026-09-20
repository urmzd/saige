// Package cache provides a response-caching decorator for types.Provider. It
// memoizes ChatStream responses keyed by a deterministic hash of
// (model, messages, tools, schema), mirroring the decorator pattern of
// agent/provider/retry and agent/provider/fallback. Only fully-completed,
// error-free streams are cached; cache hits replay recorded deltas and report a
// cache-hit UsageDelta that does not re-count tokens.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
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
	// ScopeKey identifies the tenant/auth scope. ConfigKey identifies the complete
	// immutable provider configuration, including endpoint, model options, and
	// policy revisions. Both are required for reuse across wrapper instances.
	// Without both, New uses a private instance namespace.
	ScopeKey  string
	ConfigKey string
	// CacheToolCalls opts into replaying model tool decisions. Calls still pass
	// through gates and execute again, with fresh call IDs. Disabled by default.
	CacheToolCalls bool
	// Logger and Metrics default to noop.
	Logger  *slog.Logger
	Metrics types.Metrics
}

// Provider memoizes ChatStream responses. Only fully-completed, error-free
// streams are cached.
type Provider struct {
	inner    types.Provider
	cfg      Config
	identity string
}

var (
	_ types.Provider                 = (*Provider)(nil)
	_ types.StructuredOutputProvider = (*Provider)(nil)
	_ types.NamedProvider            = (*Provider)(nil)
	_ types.ModelProvider            = (*Provider)(nil)
	_ types.ModelSwitcher            = (*Provider)(nil)
	_ types.CapabilityReporter       = (*Provider)(nil)
	_ types.ContentNegotiator        = (*Provider)(nil)
)

// New wraps a provider with response caching. cfg.Cache is required.
func New(inner types.Provider, cfg Config) *Provider {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = types.NoopMetrics{}
	}
	identity := types.NewID()
	if cfg.ScopeKey != "" && cfg.ConfigKey != "" {
		raw, _ := json.Marshal([]string{cfg.ScopeKey, cfg.ConfigKey, types.ProviderName(inner)})
		identity = string(raw)
	}
	return &Provider{inner: inner, cfg: cfg, identity: identity}
}

// Name implements types.NamedProvider.
func (p *Provider) Name() string {
	return "cache(" + types.ProviderName(p.inner) + ")"
}

// Model implements types.ModelProvider by delegating to the inner provider.
func (p *Provider) Model() string { return types.ProviderModel(p.inner) }

// WithModel implements types.ModelSwitcher: it re-targets the inner provider
// and keeps the same cache config. Without it a ConfigContent model switch was
// silently dropped under a cache decorator, and every switched request was
// answered from the original model's cache entries.
func (p *Provider) WithModel(model string) types.Provider {
	return &Provider{inner: types.ProviderWithModel(p.inner, model), cfg: p.cfg, identity: p.identity}
}

// ContentSupport implements types.ContentNegotiator by delegating to the inner
// provider, so caching an adapter does not hide its native media support.
func (p *Provider) ContentSupport() types.ContentSupport {
	return types.ProviderContentSupport(p.inner)
}

// Capabilities implements types.CapabilityReporter by delegating to the inner
// provider. A cache changes latency, not what the model accepts.
func (p *Provider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(p.inner)
	return caps
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
		return nil, fmt.Errorf("response cache: underlying provider cannot enforce a schema")
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, _ := json.Marshal([]string{p.identity, types.ProviderModel(p.inner)})
	key := p.cfg.KeyNamespace + ":" + Key(string(identity), msgs, tools, schema)

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

// NewSession preserves cache configuration while isolating inner routing state.
func (p *Provider) NewSession() types.Provider {
	inner := p.inner
	if sessions, ok := inner.(types.SessionProvider); ok {
		inner = sessions.NewSession()
	}
	return &Provider{inner: inner, cfg: p.cfg, identity: p.identity}
}
