package anthropic

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/types"
)

// PromptCachePolicy chooses where the adapter places prompt cache
// breakpoints. Anthropic caches the request prefix up to each marked block, in
// the order tools, system, messages, so each breakpoint extends the one before
// it. The API accepts at most four breakpoints; this policy places at most
// three.
//
// The cache reuses provider computation only. It is not a response cache and
// must not be used as durable conversation storage. Stable tool order and
// system text are required for reuse.
type PromptCachePolicy struct {
	// TTL is "5m" or "1h".
	TTL string
	// Tools marks the last tool definition, so the tool list stays cached
	// when the system prompt changes.
	Tools bool
	// System marks the last system block. The cached prefix includes the
	// tool definitions before it.
	System bool
	// Conversation marks the last cacheable block of a trailing user message:
	// the newest user text or the newest tool results. The next request in
	// the same conversation then reads the whole history from the cache
	// instead of only the system prompt.
	Conversation bool
}

// DefaultPromptCachePolicy marks the tools, the system prompt, and the
// trailing user message with ttl.
func DefaultPromptCachePolicy(ttl string) PromptCachePolicy {
	return PromptCachePolicy{TTL: ttl, Tools: true, System: true, Conversation: true}
}

func (p PromptCachePolicy) enabled() bool { return p.Tools || p.System || p.Conversation }

// WithPromptCachePolicy sets where prompt cache breakpoints are placed. It
// replaces an earlier WithSystemPromptCache.
func WithPromptCachePolicy(policy PromptCachePolicy) Option {
	return func(a *Adapter) { a.cachePolicy = policy }
}

// WithSystemPromptCache marks the final system block as a cache boundary.
// ttl must be "5m" or "1h". The prefix includes preceding tool definitions.
// It is WithPromptCachePolicy(PromptCachePolicy{TTL: ttl, System: true}).
func WithSystemPromptCache(ttl string) Option {
	return WithPromptCachePolicy(PromptCachePolicy{TTL: ttl, System: true})
}

// applyPromptCache places the policy's breakpoints on a complete request.
func (a *Adapter) applyPromptCache(params *anthropic.MessageNewParams) error {
	policy := a.cachePolicy
	if !policy.enabled() {
		return nil
	}
	if policy.TTL != "5m" && policy.TTL != "1h" {
		return fmt.Errorf("anthropic: invalid prompt cache TTL %q", policy.TTL)
	}
	if !a.Capabilities().Supports(types.CapPromptCacheMarkers) {
		return fmt.Errorf("anthropic: model %s does not declare prompt cache markers", a.model)
	}
	if policy.System && !policy.Tools && !policy.Conversation && len(params.System) == 0 {
		return fmt.Errorf("anthropic: system cache requires system text")
	}
	marker := anthropic.NewCacheControlEphemeralParam()
	marker.TTL = anthropic.CacheControlEphemeralTTL(policy.TTL)
	if policy.Tools && len(params.Tools) > 0 {
		if cc := params.Tools[len(params.Tools)-1].GetCacheControl(); cc != nil {
			*cc = marker
		}
	}
	if policy.System && len(params.System) > 0 {
		params.System[len(params.System)-1].CacheControl = marker
	}
	if policy.Conversation {
		markTrailingUser(params.Messages, marker)
	}
	return nil
}

// markTrailingUser marks the last cacheable block of the final message when
// it is a user message. Blocks that cannot carry a marker are skipped.
func markTrailingUser(msgs []anthropic.MessageParam, marker anthropic.CacheControlEphemeralParam) {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != anthropic.MessageParamRoleUser {
		return
	}
	blocks := msgs[len(msgs)-1].Content
	for i := len(blocks) - 1; i >= 0; i-- {
		if cc := blocks[i].GetCacheControl(); cc != nil {
			*cc = marker
			return
		}
	}
}
