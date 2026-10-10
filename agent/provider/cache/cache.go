// Package cache provides a response-caching decorator for types.Provider. It
// memoizes Stream responses keyed by a deterministic hash of
// (model, messages, tools, schema), mirroring the decorator pattern of
// agent/provider/retry and agent/provider/fallback. Only fully-completed,
// error-free streams are cached; cache hits replay recorded deltas and report a
// cache-hit UsageDelta that does not re-count tokens.
package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
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
	// SingleFlight collapses concurrent identical misses onto one upstream
	// call. Followers wait for the leader to finish, then replay its stored
	// response, so they receive no incremental streaming. A response the
	// recorder rejects (an error, a truncation, or tool calls without
	// CacheToolCalls) is not shared: each follower then calls upstream
	// itself. When the leader is cancelled, one live follower takes over.
	SingleFlight bool
	// Logger defaults to slog.Default(); Metrics defaults to noop. A failed
	// cache read is logged at Warn and recorded as "chat.cache_error", then
	// treated as a miss.
	Logger  *slog.Logger
	Metrics types.Metrics
}

// Provider memoizes Stream responses. Only fully-completed, error-free
// streams are cached.
type Provider struct {
	inner    types.Provider
	cfg      Config
	identity string
	flights  *flights
}

// flights tracks in-progress leader calls by key. It is shared by every copy
// of a Provider (WithModel, NewSession), since copies share the store.
type flights struct {
	mu       sync.Mutex
	inflight map[string]*flight
}

type flight struct {
	done     chan struct{}
	stored   bool // the leader's response was written to the store
	canceled bool // the leader's own context ended its call
}

var (
	_ types.Provider                 = (*Provider)(nil)
	_ types.StructuredOutputProvider = (*Provider)(nil)
	_ types.NamedProvider            = (*Provider)(nil)
	_ types.ModelProvider            = (*Provider)(nil)
	_ types.ModelSwitcher            = (*Provider)(nil)
	_ types.CapabilityReporter       = (*Provider)(nil)
	_ types.ContentNegotiator        = (*Provider)(nil)
	_ types.OptionsProvider          = (*Provider)(nil)
	_ types.SessionProvider          = (*Provider)(nil)
	_ types.Closer                   = (*Provider)(nil)
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
		raw, _ := json.Marshal([]string{cfg.ScopeKey, cfg.ConfigKey, types.NameOf(inner)})
		identity = string(raw)
	}
	return &Provider{inner: inner, cfg: cfg, identity: identity, flights: &flights{inflight: map[string]*flight{}}}
}

// Name implements types.NamedProvider.
func (p *Provider) Name() string {
	return "cache(" + types.NameOf(p.inner) + ")"
}

// Model implements types.ModelProvider by delegating to the inner provider.
func (p *Provider) Model() string { return types.ProviderModel(p.inner) }

// WithModel implements types.ModelSwitcher: it re-targets the inner provider
// and keeps the same cache config. Without it a ConfigPart model switch was
// silently dropped under a cache decorator, and every switched request was
// answered from the original model's cache entries.
func (p *Provider) WithModel(model string) types.Provider {
	return &Provider{inner: types.ProviderWithModel(p.inner, model), cfg: p.cfg, identity: p.identity, flights: p.flights}
}

// WithTarget implements types.TargetSwitcher: it re-targets the inner
// provider and keeps the same cache config.
func (p *Provider) WithTarget(t types.Target) (types.Provider, error) {
	inner, err := types.ProviderWithTarget(p.inner, t)
	if err != nil {
		return nil, err
	}
	return &Provider{inner: inner, cfg: p.cfg, identity: p.identity, flights: p.flights}, nil
}

// ContentSupport implements types.ContentNegotiator by delegating to the inner
// provider, so caching an adapter does not hide its native media support.
func (p *Provider) ContentSupport() types.ContentSupport {
	return types.ProviderContentSupport(p.inner)
}

// Capabilities implements types.CapabilityReporter by delegating to the inner
// provider. A cache changes latency, not what the model accepts. Capabilities
// that need request options are dropped when the inner provider cannot
// receive them.
func (p *Provider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(p.inner)
	return optionscheck.Narrow(caps, p.inner)
}

// Stream implements types.Provider. The options and schema are part of the
// cache key, so a forced tool choice never replays a response recorded
// without one. Options the inner provider cannot receive fail the call with
// types.ErrInvalidModelConfig instead of being dropped; a schema it cannot
// enforce fails the call on a miss.
func (p *Provider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil && !types.AcceptsOptions(p.inner) {
		return nil, optionscheck.Unsupported(p.inner)
	}
	call := func() (<-chan types.Delta, error) {
		if req.Schema != nil && !types.AcceptsSchema(p.inner) {
			return nil, schemacheck.Unsupported(p.inner, "provider cannot enforce a response schema")
		}
		return p.inner.Stream(ctx, req)
	}
	return p.stream(ctx, req.Messages, req.Tools, req.Schema, req.Options, call)
}

// SupportsSchema implements types.StructuredOutputProvider.
func (p *Provider) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (p *Provider) SupportsOptions() bool { return true }

// Unwrap returns the inner provider. See package wrapper.
func (p *Provider) Unwrap() types.Provider { return p.inner }

// Close implements types.Closer by closing the inner provider.
func (p *Provider) Close() error { return types.CloseProvider(p.inner) }

func (p *Provider) stream(
	ctx context.Context,
	msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema,
	opts *types.RequestOptions, call func() (<-chan types.Delta, error),
) (<-chan types.Delta, error) {
	if p.cfg.Cache == nil {
		return call() // no backing store: behave as a transparent passthrough
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parts := []string{p.identity, types.ProviderModel(p.inner)}
	if opts != nil {
		raw, err := json.Marshal(opts)
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(raw))
	}
	identity, _ := json.Marshal(parts)
	key := p.cfg.KeyNamespace + ":" + Key(string(identity), msgs, tools, schema)

	// HIT: replay recorded deltas, no upstream call.
	if cr, found := p.lookup(ctx, key); found {
		return replay(cr), nil
	}
	if !p.cfg.SingleFlight || p.flights == nil {
		return p.miss(ctx, key, call, nil)
	}
	return p.singleFlight(ctx, key, call)
}

// lookup reads key from the store. A read error is logged, recorded as a
// "chat.cache_error" metric, and treated as a miss, so a broken backend shows
// up in telemetry instead of as a silent zero hit rate.
func (p *Provider) lookup(ctx context.Context, key string) (CachedResponse, bool) {
	cr, found, err := p.cfg.Cache.Get(ctx, key)
	if err != nil {
		p.cfg.Logger.Warn("response cache get failed", "error", err)
		p.cfg.Metrics.RecordProviderCall(ctx, "chat.cache_error", p.Name(), 0, err)
		return CachedResponse{}, false
	}
	if found {
		p.cfg.Metrics.RecordProviderCall(ctx, "chat.cache_hit", p.Name(), 0, nil)
	}
	return cr, found
}

// miss calls upstream and tees the stream into a recorder. done, when
// non-nil, receives the outcome once the stream ends or the call fails.
func (p *Provider) miss(ctx context.Context, key string, call func() (<-chan types.Delta, error), done func(stored, canceled bool)) (<-chan types.Delta, error) {
	in, err := call()
	if err != nil {
		if done != nil {
			done(false, ctx.Err() != nil)
		}
		return nil, err // never cache provider construction errors
	}
	var recorded func(bool)
	if done != nil {
		recorded = func(stored bool) { done(stored, ctx.Err() != nil) }
	}
	return p.recordAndTee(ctx, key, in, recorded), nil
}

// singleFlight leads the call for key or waits for the current leader.
func (p *Provider) singleFlight(ctx context.Context, key string, call func() (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	f := p.flights
	for {
		f.mu.Lock()
		if leader, ok := f.inflight[key]; ok {
			f.mu.Unlock()
			select {
			case <-leader.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if leader.stored {
				if cr, found := p.lookup(ctx, key); found {
					return replay(cr), nil
				}
			}
			if leader.canceled && ctx.Err() == nil {
				// The leader's cancellation is its own; take over instead
				// of failing a caller whose context is still live.
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// The leader's response was not shareable; call upstream alone.
			return p.miss(ctx, key, call, nil)
		}
		mine := &flight{done: make(chan struct{})}
		f.inflight[key] = mine
		f.mu.Unlock()

		// A previous leader may have stored the response after this caller's
		// first lookup.
		if cr, found := p.lookup(ctx, key); found {
			p.finish(key, mine, true, false)
			return replay(cr), nil
		}
		return p.miss(ctx, key, call, func(stored, canceled bool) {
			p.finish(key, mine, stored, canceled)
		})
	}
}

// finish records the leader's outcome, deregisters it, and wakes followers.
func (p *Provider) finish(key string, fl *flight, stored, canceled bool) {
	f := p.flights
	f.mu.Lock()
	fl.stored, fl.canceled = stored, canceled
	if f.inflight[key] == fl {
		delete(f.inflight, key)
	}
	f.mu.Unlock()
	close(fl.done)
}

// NewSession preserves cache configuration while isolating inner routing state.
func (p *Provider) NewSession() types.Provider {
	inner := p.inner
	if sessions, ok := inner.(types.SessionProvider); ok {
		inner = sessions.NewSession()
	}
	return &Provider{inner: inner, cfg: p.cfg, identity: p.identity, flights: p.flights}
}

// EffectiveOptions implements types.OptionsReporter by forwarding to the
// inner provider.
func (p *Provider) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(p.inner)
	return o
}
