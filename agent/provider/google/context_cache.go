package google

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// ContextCache binds a provider resource to its exact model, message prefix,
// and tools. Treat the handle as tenant-scoped configuration, not portable data.
// Persist it with the run configuration if the resource must be reused later.
type ContextCache struct {
	Name        string    `json:"name"`
	Model       string    `json:"model"`
	PrefixCount int       `json:"prefix_count"`
	Fingerprint string    `json:"fingerprint"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// WithContextCache binds an existing resource. A changed prefix, tool set,
// model, or expired handle fails locally rather than silently using stale data.
func WithContextCache(cache ContextCache) Option {
	return func(a *Adapter) { a.contextCache = &cache }
}

// CreateContextCache explicitly creates a billable provider cache resource.
// It does not bind the adapter or create anything on cache misses. TTL must be
// positive. Storage and creation charges are not included in types.Budget.
func (a *Adapter) CreateContextCache(ctx context.Context, prefix []types.Message, tools []types.ToolDef, ttl time.Duration) (ContextCache, error) {
	if ttl <= 0 || len(prefix) == 0 {
		return ContextCache{}, fmt.Errorf("google: cache requires a prefix and positive TTL")
	}
	if !a.Capabilities().Supports(types.CapExplicitContextCache) {
		return ContextCache{}, fmt.Errorf("google: model %s does not declare explicit context caching", a.model)
	}
	contents, config := a.buildRequest(prefix, tools)
	fingerprint, err := cacheFingerprint(contents, config)
	if err != nil {
		return ContextCache{}, err
	}
	resource, err := a.client.Caches.Create(ctx, a.model, &genai.CreateCachedContentConfig{
		TTL: ttl, Contents: contents, SystemInstruction: config.SystemInstruction, Tools: config.Tools,
	})
	if err != nil {
		return ContextCache{}, err
	}
	return ContextCache{Name: resource.Name, Model: a.model, PrefixCount: len(prefix), Fingerprint: fingerprint, ExpiresAt: resource.ExpireTime}, nil
}

// RefreshContextCache extends provider retention and returns a new handle.
// Existing adapters remain unchanged; bind the returned handle explicitly.
func (a *Adapter) RefreshContextCache(ctx context.Context, cache ContextCache, ttl time.Duration) (ContextCache, error) {
	if cache.Model != a.model || cache.Name == "" || ttl <= 0 {
		return ContextCache{}, fmt.Errorf("google: invalid cache refresh")
	}
	resource, err := a.client.Caches.Update(ctx, cache.Name, &genai.UpdateCachedContentConfig{TTL: ttl})
	if err != nil {
		return ContextCache{}, err
	}
	cache.ExpiresAt = resource.ExpireTime
	return cache, nil
}

func (a *Adapter) DeleteContextCache(ctx context.Context, cache ContextCache) error {
	if cache.Model != a.model || cache.Name == "" {
		return fmt.Errorf("google: invalid cache deletion")
	}
	_, err := a.client.Caches.Delete(ctx, cache.Name, nil)
	return err
}

func cacheFingerprint(contents []*genai.Content, config *genai.GenerateContentConfig) (string, error) {
	raw, err := json.Marshal(struct {
		Contents []*genai.Content
		System   *genai.Content
		Tools    []*genai.Tool
	}{contents, config.SystemInstruction, config.Tools})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func (a *Adapter) cachedRequest(messages []types.Message, tools []types.ToolDef) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	cache := a.contextCache
	if cache == nil {
		contents, config := a.buildRequest(messages, tools)
		return contents, config, nil
	}
	if cache.Name == "" || cache.Model != a.model || cache.PrefixCount <= 0 || cache.PrefixCount > len(messages) {
		return nil, nil, fmt.Errorf("google: context cache does not match this model or prefix")
	}
	if !cache.ExpiresAt.IsZero() && !time.Now().Before(cache.ExpiresAt) {
		return nil, nil, fmt.Errorf("google: context cache has expired")
	}
	prefix, prefixConfig := a.buildRequest(messages[:cache.PrefixCount], tools)
	fingerprint, err := cacheFingerprint(prefix, prefixConfig)
	if err != nil {
		return nil, nil, err
	}
	if fingerprint != cache.Fingerprint {
		return nil, nil, fmt.Errorf("google: context cache prefix or tools changed")
	}
	contents, config := a.buildRequest(messages[cache.PrefixCount:], nil)
	if config.SystemInstruction != nil {
		return nil, nil, fmt.Errorf("google: system instructions cannot change after the cached prefix")
	}
	config.CachedContent = cache.Name
	config.Tools = nil // tools and system instructions belong to the resource
	return contents, config, nil
}
