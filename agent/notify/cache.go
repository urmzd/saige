package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// CacheConfig configures a Cache.
type CacheConfig[V any] struct {
	// Local is this process's cache, such as agent/cache/memcache. Required.
	Local types.Cache[V]
	// Shared is the store every process reads, such as a
	// postgres.CacheStore behind cache.BytesCache or toolcache.BytesCache.
	// Nil keeps values only in Local; invalidation still crosses processes.
	Shared types.Cache[V]
	// Notifier carries invalidations between processes. Required.
	Notifier types.Notifier
	// Channel is the invalidation channel. Every process sharing a store uses
	// the same channel. Required.
	Channel string
	// LocalTTL bounds how long a value read from Shared stays in Local. Zero
	// uses the TTL passed to Set, or Local's default for values read from
	// Shared.
	LocalTTL time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Cache is a two-level types.Cache whose local level stays coherent across
// processes. Set and Delete write Shared, then Local, then publish the key;
// every other process drops the key from its Local on receipt. Reads try
// Local first and fill it from Shared.
//
// Use it as the Cache of a response cache or a tool cache:
//
//	inv, err := notify.NewCache(ctx, notify.CacheConfig[cache.CachedResponse]{
//		Local:    memcache.New[cache.CachedResponse](),
//		Shared:   cache.BytesCache(pgStore),
//		Notifier: pgNotifier,
//		Channel:  "saige.cache.response",
//	})
//
// When the subscription ends because the notifier closed, Local can no
// longer be trusted, so Cache bypasses it until Close.
type Cache[V any] struct {
	cfg      CacheConfig[V]
	origin   []byte
	cancel   func()
	done     chan struct{}
	degraded atomic.Bool
	// generation counts received invalidations. A fill from Shared is
	// skipped when one arrived during the read, since the value read may be
	// the one it invalidated.
	generation atomic.Uint64
	// fill orders a local fill against an invalidation, so a fill that
	// passed the generation check finishes before the delete runs.
	fill sync.Mutex
}

var _ types.Cache[int] = (*Cache[int])(nil)

// NewCache subscribes to the invalidation channel and returns the cache.
// Close it to stop the subscription.
func NewCache[V any](ctx context.Context, cfg CacheConfig[V]) (*Cache[V], error) {
	if cfg.Local == nil || cfg.Notifier == nil {
		return nil, errors.New("notify: cache requires Local and Notifier")
	}
	if err := types.ValidateChannel(cfg.Channel); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ch, cancel, err := cfg.Notifier.Subscribe(context.WithoutCancel(ctx), cfg.Channel)
	if err != nil {
		return nil, err
	}
	c := &Cache[V]{cfg: cfg, origin: []byte(types.NewID() + "\n"), cancel: cancel, done: make(chan struct{})}
	go c.listen(ch)
	return c, nil
}

func (c *Cache[V]) listen(ch <-chan types.Notification) {
	defer close(c.done)
	for msg := range ch {
		if bytes.HasPrefix(msg.Payload, c.origin) {
			continue // this process already applied its own write
		}
		_, key, ok := bytes.Cut(msg.Payload, []byte("\n"))
		if !ok {
			continue
		}
		c.fill.Lock()
		c.generation.Add(1)
		err := c.cfg.Local.Delete(context.Background(), string(key))
		c.fill.Unlock()
		if err != nil {
			c.cfg.Logger.Warn("notify: local cache invalidation failed", "channel", c.cfg.Channel, "err", err)
			c.degraded.Store(true)
		}
	}
	// The subscription ended without Close: stop trusting Local.
	c.degraded.Store(true)
}

// Get implements types.Cache.
func (c *Cache[V]) Get(ctx context.Context, key string) (V, bool, error) {
	if !c.degraded.Load() {
		if v, found, err := c.cfg.Local.Get(ctx, key); err == nil && found {
			return v, true, nil
		}
	}
	var zero V
	if c.cfg.Shared == nil {
		return zero, false, nil
	}
	gen := c.generation.Load()
	v, found, err := c.cfg.Shared.Get(ctx, key)
	if err != nil || !found {
		return zero, false, err
	}
	c.fill.Lock()
	if !c.degraded.Load() && c.generation.Load() == gen {
		_ = c.cfg.Local.Set(ctx, key, v, c.cfg.LocalTTL)
	}
	c.fill.Unlock()
	return v, true, nil
}

// Set implements types.Cache. The value is written to Shared and Local, and
// other processes drop their local copy of key.
func (c *Cache[V]) Set(ctx context.Context, key string, value V, ttl time.Duration) error {
	if c.cfg.Shared != nil {
		if err := c.cfg.Shared.Set(ctx, key, value, ttl); err != nil {
			return err
		}
	}
	localTTL := ttl
	if c.cfg.LocalTTL > 0 && (localTTL <= 0 || c.cfg.LocalTTL < localTTL) {
		localTTL = c.cfg.LocalTTL
	}
	if err := c.cfg.Local.Set(ctx, key, value, localTTL); err != nil {
		return err
	}
	return c.publish(ctx, key)
}

// Delete implements types.Cache. It removes key from Shared and Local and
// from every other process's Local.
func (c *Cache[V]) Delete(ctx context.Context, key string) error {
	if c.cfg.Shared != nil {
		if err := c.cfg.Shared.Delete(ctx, key); err != nil {
			return err
		}
	}
	if err := c.cfg.Local.Delete(ctx, key); err != nil {
		return err
	}
	return c.publish(ctx, key)
}

// Invalidate is Delete under the name callers use for an external change.
func (c *Cache[V]) Invalidate(ctx context.Context, key string) error { return c.Delete(ctx, key) }

func (c *Cache[V]) publish(ctx context.Context, key string) error {
	payload := make([]byte, 0, len(c.origin)+len(key))
	payload = append(append(payload, c.origin...), key...)
	return c.cfg.Notifier.Publish(ctx, c.cfg.Channel, payload)
}

// Close stops the invalidation subscription and waits for it to end. It
// does not close Local, Shared or the notifier.
func (c *Cache[V]) Close() error {
	c.cancel()
	<-c.done
	return nil
}
