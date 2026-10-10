package catalog

import (
	"fmt"
	"slices"
	"sync"

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
	providerAnthropic types.ProviderName = "anthropic"
	providerOpenAI    types.ProviderName = "openai"
	providerGoogle    types.ProviderName = "google"
	providerOllama    types.ProviderName = "ollama"
	optReasoningOn                       = "reasoning_enabled"
)

// ExpressError reports a control an adapter has no way to send. It names the
// option and why, without the model, so a caller can wrap it with
// ModelCapabilities.OptionError.
type ExpressError struct {
	Provider types.ProviderName
	Option   string
	Reason   string
}

func (e *ExpressError) Error() string {
	return fmt.Sprintf("%s: %s", e.Option, e.Reason)
}

// notExpressible is the reason used for a control an adapter has no option for.
func notExpressible(provider types.ProviderName, option string) *ExpressError {
	return &ExpressError{Provider: provider, Option: option, Reason: "not expressible by the " + string(provider) + " adapter"}
}

// Expressible reports the first option in o that the named adapter cannot
// send. It is the single table provider.Build and catalog validation share:
// an option that fails here is rejected, never dropped. Unknown providers
// pass, since their adapters are not built here.
func Expressible(provider types.ProviderName, o types.RequestOptions) error {
	if sp, ok := surfaceOf(provider); ok && sp.Expressible != nil {
		return sp.Expressible(o)
	}
	return builtinExpressible(provider, o)
}

func builtinExpressible(provider types.ProviderName, o types.RequestOptions) error {
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
func ExpressibleServerTools(provider types.ProviderName) error {
	if sp, ok := surfaceOf(provider); ok && sp.ServerTools != nil {
		if *sp.ServerTools {
			return nil
		}
		return notExpressible(provider, "server_tools")
	}
	switch provider {
	case providerOpenAI, providerOllama:
		return notExpressible(provider, "server_tools")
	}
	return nil
}

// ExpressiblePromptCache reports whether the named adapter can apply a
// prompt cache mode. Off always passes.
func ExpressiblePromptCache(provider types.ProviderName, mode string) error {
	if sp, ok := surfaceOf(provider); ok && sp.PromptCacheModes != nil && mode != "" && mode != PromptCacheOff {
		if slices.Contains(sp.PromptCacheModes, mode) {
			return nil
		}
		return notExpressible(provider, "prompt_cache."+mode)
	}
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

// SurfaceSpec is what an adapter can send on one API surface. An adapter
// package registers its own with RegisterSurface; without one, the rules
// this package has always applied hold.
type SurfaceSpec struct {
	// Name is the endpoint surface, such as types.SurfaceOpenAIChat.
	Name string
	// Provider is the adapter that serves the surface.
	Provider types.ProviderName
	// Expressible reports the first option the adapter cannot send, as an
	// *ExpressError. Nil keeps the built-in rule.
	Expressible func(types.RequestOptions) error
	// ServerTools says whether the adapter sends provider-executed tools.
	// Nil keeps the built-in rule.
	ServerTools *bool
	// PromptCacheModes lists the prompt cache modes besides off the adapter
	// applies. Nil keeps the built-in rule.
	PromptCacheModes []string
}

var (
	surfaceMu sync.RWMutex
	surfaces  = map[string]SurfaceSpec{}
)

// RegisterSurface records what an adapter can send on a surface. A later
// registration of the same name replaces the earlier one.
func RegisterSurface(s SurfaceSpec) {
	surfaceMu.Lock()
	defer surfaceMu.Unlock()
	surfaces[s.Name] = s
}

// Surface returns a registered surface.
func Surface(name string) (SurfaceSpec, bool) {
	surfaceMu.RLock()
	defer surfaceMu.RUnlock()
	s, ok := surfaces[name]
	return s, ok
}

// defaultSurface is the surface a provider's adapter speaks by default.
func defaultSurface(provider types.ProviderName) string {
	switch provider {
	case providerAnthropic:
		return types.SurfaceAnthropicMessages
	case providerOpenAI:
		return types.SurfaceOpenAIChat
	case providerGoogle:
		return types.SurfaceGeminiAPI
	case providerOllama:
		return types.SurfaceOllamaNative
	}
	return string(provider)
}

// surfaceOf returns the registered surface a provider's adapter speaks by
// default.
func surfaceOf(provider types.ProviderName) (SurfaceSpec, bool) {
	return Surface(defaultSurface(provider))
}

// SurfaceProvider names the adapter that serves an endpoint surface.
func SurfaceProvider(surface string) (types.ProviderName, bool) {
	if s, ok := Surface(surface); ok && s.Provider != "" {
		return s.Provider, true
	}
	switch surface {
	case types.SurfaceAnthropicMessages:
		return providerAnthropic, true
	case types.SurfaceOpenAIChat, types.SurfaceOpenAIResponses, types.SurfaceOpenAICompatible:
		return providerOpenAI, true
	case types.SurfaceGeminiAPI, types.SurfaceVertex:
		return providerGoogle, true
	case types.SurfaceOllamaNative:
		return providerOllama, true
	}
	return "", false
}

// KnownSurfaces lists the built-in endpoint surfaces.
func KnownSurfaces() []string {
	return []string{types.SurfaceAnthropicMessages, types.SurfaceOpenAIChat, types.SurfaceOpenAIResponses,
		types.SurfaceOpenAICompatible, types.SurfaceGeminiAPI, types.SurfaceVertex, types.SurfaceOllamaNative}
}
