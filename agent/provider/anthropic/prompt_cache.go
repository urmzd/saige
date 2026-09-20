package anthropic

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/types"
)

// WithSystemPromptCache marks the final system block as a cache boundary.
// ttl must be "5m" or "1h". The prefix includes preceding tool definitions.
// Stable tool order and system text are required for reuse. It is not a
// response cache and must not be used as durable conversation storage.
func WithSystemPromptCache(ttl string) Option {
	return func(a *Adapter) { a.systemCacheTTL = ttl }
}

func (a *Adapter) applyPromptCache(blocks []anthropic.TextBlockParam) error {
	if a.systemCacheTTL == "" {
		return nil
	}
	if a.systemCacheTTL != "5m" && a.systemCacheTTL != "1h" {
		return fmt.Errorf("anthropic: invalid prompt cache TTL %q", a.systemCacheTTL)
	}
	if !a.Capabilities().Supports(types.CapPromptCacheMarkers) {
		return fmt.Errorf("anthropic: model %s does not declare prompt cache markers", a.model)
	}
	if len(blocks) == 0 {
		return fmt.Errorf("anthropic: system cache requires system text")
	}
	marker := anthropic.NewCacheControlEphemeralParam()
	marker.TTL = anthropic.CacheControlEphemeralTTL(a.systemCacheTTL)
	blocks[len(blocks)-1].CacheControl = marker
	return nil
}
