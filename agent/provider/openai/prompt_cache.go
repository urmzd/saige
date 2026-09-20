package openai

import (
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/types"
)

const promptCacheInMemory = "in_memory"

// WithPromptCache sets provider-side prefix cache affinity and retention.
// retention is empty, "in_memory", or "24h". Model and endpoint availability
// still apply. This does not cache responses or guarantee a cache hit.
func WithPromptCache(key, retention string) Option {
	return func(c *config) { c.params.cacheKey, c.params.cacheRetention = key, retention }
}

func (a *Adapter) applyPromptCache(p *openai.ChatCompletionNewParams) error {
	key, retention := a.params.cacheKey, a.params.cacheRetention
	if key == "" && retention == "" {
		return nil
	}
	if !a.Capabilities().Supports(types.CapAutomaticPromptCache) {
		return fmt.Errorf("openai: model %s does not declare automatic prompt caching", a.model)
	}
	if retention == "in-memory" {
		retention = promptCacheInMemory
	} // older SDK spelling
	switch retention {
	case "", promptCacheInMemory, "24h":
	default:
		return fmt.Errorf("openai: invalid prompt cache retention %q", retention)
	}
	if key != "" {
		p.PromptCacheKey = openai.String(key)
	}
	p.PromptCacheRetention = openai.ChatCompletionNewParamsPromptCacheRetention(retention)
	return nil
}
