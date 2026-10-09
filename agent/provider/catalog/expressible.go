package catalog

import (
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// Prompt cache modes accepted by the catalog's prompt_cache object and by
// provider.Config.PromptCache.
const (
	// PromptCacheOff sends no cache controls. Every adapter accepts it.
	PromptCacheOff = "off"
	// PromptCacheMarkers places explicit cache breakpoints (Anthropic).
	PromptCacheMarkers = "markers"
	// PromptCacheAutomatic sets a cache key and retention on a provider that
	// caches prefixes automatically (OpenAI).
	PromptCacheAutomatic = "automatic"
)

const (
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
	providerGoogle    = "google"
	providerOllama    = "ollama"
	optReasoningOn    = "reasoning_enabled"
)

// ExpressError reports a control an adapter has no way to send. It names the
// option and why, without the model, so a caller can wrap it with
// ModelCapabilities.OptionError.
type ExpressError struct {
	Provider string
	Option   string
	Reason   string
}

func (e *ExpressError) Error() string {
	return fmt.Sprintf("%s: %s", e.Option, e.Reason)
}

// notExpressible is the reason used for a control an adapter has no option for.
func notExpressible(provider, option string) *ExpressError {
	return &ExpressError{Provider: provider, Option: option, Reason: "not expressible by the " + provider + " adapter"}
}

// Expressible reports the first option in o that the named adapter cannot
// send. It is the single table provider.Build and catalog validation share:
// an option that fails here is rejected, never dropped. Unknown providers
// pass, since their adapters are not built here.
func Expressible(provider string, o types.RequestOptions) error {
	switch provider {
	case providerAnthropic:
		switch {
		case o.Seed != nil:
			return notExpressible(provider, "seed")
		case o.FrequencyPenalty != nil:
			return notExpressible(provider, "frequency_penalty")
		case o.PresencePenalty != nil:
			return notExpressible(provider, "presence_penalty")
		case o.ReasoningEnabled != nil:
			return &ExpressError{Provider: provider, Option: optReasoningOn, Reason: "use a reasoning budget or effort"}
		}
	case providerOpenAI:
		switch {
		case o.TopK != nil:
			return notExpressible(provider, "top_k")
		case o.ReasoningBudget != nil:
			return notExpressible(provider, "reasoning_budget")
		case o.ReasoningEnabled != nil:
			return &ExpressError{Provider: provider, Option: optReasoningOn, Reason: "use a reasoning effort"}
		}
	case providerGoogle:
		switch {
		case o.ParallelTools != nil:
			return notExpressible(provider, "parallel_tools")
		case o.ReasoningEnabled != nil && *o.ReasoningEnabled:
			return &ExpressError{Provider: provider, Option: optReasoningOn, Reason: "use a reasoning budget or effort"}
		}
	case providerOllama:
		switch {
		case o.FrequencyPenalty != nil:
			return notExpressible(provider, "frequency_penalty")
		case o.PresencePenalty != nil:
			return notExpressible(provider, "presence_penalty")
		case o.ParallelTools != nil:
			return notExpressible(provider, "parallel_tools")
		case o.ReasoningBudget != nil:
			return notExpressible(provider, "reasoning_budget")
		case o.ReasoningEffort != nil:
			return notExpressible(provider, "reasoning_effort")
		}
	}
	return nil
}

// ExpressibleServerTools reports whether the named adapter sends
// provider-executed tools. The OpenAI chat and Ollama adapters do not.
func ExpressibleServerTools(provider string) error {
	switch provider {
	case providerOpenAI, providerOllama:
		return notExpressible(provider, "server_tools")
	}
	return nil
}

// ExpressiblePromptCache reports whether the named adapter can apply a
// prompt cache mode. Off always passes.
func ExpressiblePromptCache(provider, mode string) error {
	switch mode {
	case "", PromptCacheOff:
		return nil
	case PromptCacheMarkers:
		if provider == providerAnthropic {
			return nil
		}
	case PromptCacheAutomatic:
		if provider == providerOpenAI {
			return nil
		}
	default:
		return &ExpressError{Provider: provider, Option: "prompt_cache.mode", Reason: fmt.Sprintf("unknown mode %q", mode)}
	}
	return notExpressible(provider, "prompt_cache."+mode)
}
