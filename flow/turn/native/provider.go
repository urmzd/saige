package native

import (
	"fmt"
	"os"
	"slices"
	"strings"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/fallback"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/provider/retry"
)

// Provider names a spec may ask for. The set is open: a deployment adds its
// own with WithProvider, and a spec naming a provider the process has not
// registered fails at build time with a message that lists what is available,
// rather than silently falling back to a model nobody asked for.
const (
	// ProviderAnthropic reads ANTHROPIC_API_KEY (see EnvAnthropicKey).
	ProviderAnthropic = "anthropic"
	// ProviderOpenAI speaks the OpenAI Chat Completions API: OpenAI itself,
	// or any server that implements the same API (Ollama, vLLM, LM Studio, llama.cpp,
	// OpenRouter, Groq, Together, a gateway) when EnvOpenAIBaseURL points at
	// it. It is one OpenAICompatible endpoint; register more with
	// WithOpenAICompatible.
	ProviderOpenAI = "openai"
	// ProviderOffline replays a canned script instead of calling a model.
	// It is what makes an flow deployment demonstrable with no API key,
	// no daemon, and no network: the whole path from tag to delivered event
	// runs, and only the token source is fake. Tests use it with WithScript.
	ProviderOffline = "offline"
)

// Environment variables the built-in providers read. They are read at build
// time, not at init, so a process can set them after start (a config loader, a
// secret fetch) and so a test can set one per case.
const (
	EnvAnthropicKey = "ANTHROPIC_API_KEY"
	// EnvOpenAIKey authenticates the openai provider. It is required for
	// the OpenAI API and optional for a compatible server, many of which
	// run locally with no key.
	EnvOpenAIKey = "OPENAI_API_KEY"
	// EnvOpenAIBaseURL points the openai provider at an OpenAI-compatible
	// server, for example http://localhost:8000/v1. Unset is OpenAI.
	EnvOpenAIBaseURL = "OPENAI_BASE_URL"
)

// DefaultOpenAIBaseURL is the OpenAI API, used when no base URL is given.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// ProviderFunc builds the LLM provider a spec asks for.
//
// It takes the whole Spec rather than just the model name because a provider
// may need more of the definition than the model: a thinking budget derived
// from the agent's purpose, a per-agent endpoint, a fake keyed to the agent's
// name. It returns an error rather than panicking or falling back, since a run
// against the wrong model is worse than a run that does not start.
type ProviderFunc func(spec Spec) (saigetypes.Provider, error)

// ProviderResolver finds the factory for a provider no option registered by
// name, when a turn runs: one a deployment adds while it runs. It reports
// false for a name it does not know, and the turn fails as it would for any
// unknown provider.
type ProviderResolver func(name string) (ProviderFunc, bool)

// defaultProviders returns the built-in provider table. It is a function, not a
// package-level map, so no two Runners can mutate each other's table and so the
// zero configuration is not process-global state.
func defaultProviders() map[string]ProviderFunc {
	return map[string]ProviderFunc{
		ProviderAnthropic: nil, // resolved per Runner: it needs the cache lifetime
		ProviderOpenAI:    openaiFromEnv,
		ProviderOffline:   nil, // resolved per Runner: it needs the script
	}
}

// anthropicProvider builds the Anthropic provider with prompt caching at the
// given lifetime (see WithPromptCache).
func anthropicProvider(cacheTTL string) ProviderFunc {
	return func(spec Spec) (saigetypes.Provider, error) {
		key := os.Getenv(EnvAnthropicKey)
		if key == "" {
			return nil, fmt.Errorf("%w: agent %q asks for the %s provider but %s is unset", ErrInvalid, spec.Name, ProviderAnthropic, EnvAnthropicKey)
		}
		if spec.Model == "" {
			return nil, fmt.Errorf("%w: agent %q asks for the %s provider with no model", ErrInvalid, spec.Name, ProviderAnthropic)
		}
		var opts []anthropic.Option
		if spec.Effort != "" {
			opts = append(opts, anthropic.WithReasoningEffort(spec.Effort))
		}
		a, err := newAnthropic(key, spec.Model, cacheTTL, opts...)
		if err != nil {
			return nil, fmt.Errorf("%w: agent %q: %v", ErrInvalid, spec.Name, err)
		}
		if err := checkEffort(spec, a.Capabilities()); err != nil {
			return nil, err
		}
		return withRetry(a)
	}
}

// checkEffort refuses a reasoning effort the model does not take. The
// adapters check it per request and fail the request, so an effort a model
// cannot honor would build cleanly and fail every call; refusing it here
// fails the turn before any model call, with a message that lists what the
// model takes.
func checkEffort(spec Spec, caps saigetypes.ModelCapabilities) error {
	if spec.Effort == "" {
		return nil
	}
	if !caps.Supports(saigetypes.CapReasoningEffort) || len(caps.ReasoningEfforts) == 0 {
		return fmt.Errorf("%w: agent %q asks %s %q for reasoning effort %q; the model takes no effort",
			ErrInvalid, spec.Name, spec.Provider, spec.Model, spec.Effort)
	}
	if !slices.Contains(caps.ReasoningEfforts, spec.Effort) {
		return fmt.Errorf("%w: agent %q asks %s %q for reasoning effort %q; the model takes %s",
			ErrInvalid, spec.Name, spec.Provider, spec.Model, spec.Effort, strings.Join(caps.ReasoningEfforts, ", "))
	}
	return nil
}

// newAnthropic builds the adapter, with cache breakpoints when the model
// supports them.
//
// The capability is checked here rather than left to the adapter because the
// adapter checks it per request and fails the request: an agent pinned to a
// model without cache breakpoints would build cleanly and then fail every
// call. Caching is an optimization, so a model that cannot do it runs
// uncached instead.
//
// The policy is saige's default: a breakpoint after the tools, after the
// system prompt, and on the trailing message. Anthropic orders a request
// tools, system, messages, which is the stable-to-volatile order this package
// lays a turn out in, so each breakpoint ends a longer stable prefix than the
// one before it.
func newAnthropic(key, model, cacheTTL string, opts ...anthropic.Option) (*anthropic.Adapter, error) {
	cfg := anthropic.Config{APIKey: key, Model: saigetypes.ModelID(model)}
	a, err := anthropic.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	if cacheTTL == PromptCacheOff || !a.Capabilities().Supports(saigetypes.CapPromptCacheMarkers) {
		return a, nil
	}
	return anthropic.New(cfg, append(opts[:len(opts):len(opts)],
		anthropic.WithPromptCachePolicy(anthropic.DefaultPromptCachePolicy(cacheTTL)))...)
}

// OpenAICompatible is a model endpoint that speaks the OpenAI Chat
// Completions API, registered under a provider name so a spec can ask for it:
// OpenAI itself, or vLLM, LM Studio, llama.cpp, OpenRouter, Groq, a gateway.
//
// The built-in "openai" provider is one of these, configured from
// EnvOpenAIBaseURL and EnvOpenAIKey. A deployment that talks to more servers
// registers each under its own name with WithOpenAICompatible:
//
//	native.WithOpenAICompatible(native.OpenAICompatible{
//		Name:    "vllm",
//		BaseURL: "http://gpu-box:8000/v1",
//	})
//
// The fields are the whole configuration. OpenAI's SDK also reads
// OPENAI_BASE_URL and OPENAI_API_KEY from the environment, and both are
// overridden here, so an endpoint registered for one server never sends the
// OpenAI key to it or follows the environment somewhere else. The SDK's other
// environment settings (OPENAI_ORG_ID, OPENAI_PROJECT_ID,
// OPENAI_CUSTOM_HEADERS) still apply to every client in the process; leave
// them unset in a deployment that also talks to third-party servers.
//
// Two things differ from the Anthropic provider. There are no cache
// breakpoints to place: OpenAI caches a request's prefix on its own, which the
// stable-to-volatile layout already serves, and a compatible server caches or
// not as it chooses. And a model saige's catalog does not know (most models a
// compatible server hosts) resolves to conservative capabilities and no price,
// so its usage reports tokens and no cost.
type OpenAICompatible struct {
	// Name is the provider name a spec asks for, such as "openai" or
	// "vllm". Required.
	Name string
	// BaseURL is where the API is served, such as
	// "http://localhost:8000/v1". Empty is DefaultOpenAIBaseURL.
	BaseURL string
	// KeyEnv names the environment variable that holds the API key. It is
	// read when an agent is built, never stored here, so a key rotated in
	// the environment is picked up by the next run. When it is set the key
	// is required; when it is empty no key is sent, which is what a local
	// server expects. OpenAI itself always needs one.
	KeyEnv string
}

// WithOpenAICompatible registers each endpoint as a provider under its Name,
// replacing any provider of that name.
func WithOpenAICompatible(endpoints ...OpenAICompatible) Option {
	return func(c *config) {
		for _, e := range endpoints {
			if e.Name != "" {
				c.providers[e.Name] = e.Provider()
			}
		}
	}
}

// Provider returns the endpoint as a ProviderFunc.
func (e OpenAICompatible) Provider() ProviderFunc {
	return func(spec Spec) (saigetypes.Provider, error) {
		if spec.Model == "" {
			return nil, fmt.Errorf("%w: agent %q asks for the %s provider with no model", ErrInvalid, spec.Name, e.Name)
		}
		base := e.BaseURL
		if base == "" {
			base = DefaultOpenAIBaseURL
		}
		var key string
		if e.KeyEnv != "" {
			if key = os.Getenv(e.KeyEnv); key == "" {
				return nil, fmt.Errorf("%w: agent %q asks for the %s provider but %s is unset", ErrInvalid, spec.Name, e.Name, e.KeyEnv)
			}
		}
		if key == "" && base == DefaultOpenAIBaseURL {
			return nil, fmt.Errorf("%w: agent %q asks for the %s provider, which is OpenAI, with no key", ErrInvalid, spec.Name, e.Name)
		}
		opts := []openai.Option{openai.WithBaseURL(base)}
		if spec.Effort != "" {
			opts = append(opts, openai.WithReasoningEffort(spec.Effort))
		}
		a, err := openai.New(openai.Config{APIKey: key, Model: saigetypes.ModelID(spec.Model)}, opts...)
		if err != nil {
			return nil, fmt.Errorf("%w: agent %q: %v", ErrInvalid, spec.Name, err)
		}
		if err := checkEffort(spec, a.Capabilities()); err != nil {
			return nil, err
		}
		return withRetry(a)
	}
}

// openaiFromEnv is the built-in "openai" endpoint, read from the environment
// at build time like every built-in provider: OpenAI with EnvOpenAIKey, or
// the compatible server at EnvOpenAIBaseURL, with EnvOpenAIKey if it is set.
func openaiFromEnv(spec Spec) (saigetypes.Provider, error) {
	e := OpenAICompatible{Name: ProviderOpenAI, BaseURL: os.Getenv(EnvOpenAIBaseURL), KeyEnv: EnvOpenAIKey}
	if e.BaseURL != "" && os.Getenv(EnvOpenAIKey) == "" {
		e.KeyEnv = "" // a local server needs no key
	}
	if e.BaseURL == "" && os.Getenv(EnvOpenAIKey) == "" {
		return nil, fmt.Errorf("%w: agent %q asks for the %s provider but %s is unset (set %s to use a compatible server instead)",
			ErrInvalid, spec.Name, ProviderOpenAI, EnvOpenAIKey, EnvOpenAIBaseURL)
	}
	return e.Provider()(spec)
}

// Conditions a spec's FallbackOn may name. They mirror the policy's (definition.FallbackConditions), and each
// is one of saige's error classifications. Whatever is named, an error no
// other model can fix never falls through: see fallbackRule.
const (
	// FallbackOnAny is fallback.DefaultFallbackOn, and what an empty
	// FallbackOn means. It stands alone.
	FallbackOnAny = "any"
	// FallbackOnUnavailable is saigetypes.IsUnavailable: an outage.
	FallbackOnUnavailable = "unavailable"
	// FallbackOnRateLimit is saigetypes.IsRateLimit.
	FallbackOnRateLimit = "rate_limit"
	// FallbackOnTransient is saigetypes.IsTransient: a timeout or reset
	// connection, and also a rate limit or an outage.
	FallbackOnTransient = "transient"
	// FallbackOnAuth is saigetypes.IsAuth.
	FallbackOnAuth = "auth"
	// FallbackOnContextLength is saigetypes.IsContextLength.
	FallbackOnContextLength = "context_length"
	// FallbackOnContentFilter is saigetypes.IsContentFilter.
	FallbackOnContentFilter = "content_filter"
)

// fallbackClasses maps each condition but FallbackOnAny to saige's test.
var fallbackClasses = map[string]func(error) bool{
	FallbackOnUnavailable:   saigetypes.IsUnavailable,
	FallbackOnRateLimit:     saigetypes.IsRateLimit,
	FallbackOnTransient:     saigetypes.IsTransient,
	FallbackOnAuth:          saigetypes.IsAuth,
	FallbackOnContextLength: saigetypes.IsContextLength,
	FallbackOnContentFilter: saigetypes.IsContentFilter,
}

// FallbackConditions returns every condition FallbackOn may name, sorted.
func FallbackConditions() []string {
	out := []string{FallbackOnAny}
	for c := range fallbackClasses {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// fallbackRule turns a spec's FallbackOn into the predicate saige's fallback
// provider asks of each failed call. Nil means fallback.DefaultFallbackOn.
// A named class narrows that default and never widens it: an error is
// passed on only when the default would pass it on (so a cancelled run, an
// invalid request or an exhausted budget never is) and it is of a named
// class. An unknown condition is refused rather than ignored, since ignoring
// it would fall back more or less often than the spec says.
func fallbackRule(spec Spec) (func(error) bool, error) {
	if len(spec.FallbackOn) == 0 {
		return nil, nil
	}
	if len(spec.Fallbacks) == 0 {
		return nil, fmt.Errorf("%w: agent %q says when to fall back but names no fallback models", ErrInvalid, spec.Name)
	}
	if len(spec.FallbackOn) == 1 && spec.FallbackOn[0] == FallbackOnAny {
		return nil, nil
	}
	tests := make([]func(error) bool, 0, len(spec.FallbackOn))
	for _, c := range spec.FallbackOn {
		test, ok := fallbackClasses[c]
		if !ok {
			return nil, fmt.Errorf("%w: agent %q falls back on %q; the conditions are %s (%q alone)",
				ErrInvalid, spec.Name, c, strings.Join(FallbackConditions(), ", "), FallbackOnAny)
		}
		tests = append(tests, test)
	}
	return func(err error) bool {
		if !fallback.DefaultFallbackOn(err) {
			return false
		}
		for _, test := range tests {
			if test(err) {
				return true
			}
		}
		return false
	}, nil
}

// modelChain builds the provider a revision's turns call, and the members
// behind it, primary first.
//
// Each model, the primary and every fallback, is built through the same
// provider table from a copy of the spec that names that model alone, so a
// ProviderFunc builds one model and never sees the policy around it. A
// fallback this process cannot build fails the build exactly as the primary
// would: an agent that reads as having a safety net and silently does not is
// worse than one that does not start.
//
// With fallbacks the members are composed with saige's fallback provider
// under the spec's FallbackOn (see fallbackRule). Its default, an empty
// FallbackOn, moves a call on to the next model on an outage, a rate limit,
// an auth or context-length failure, and stops on a cancelled run, an
// invalid request, or an exhausted budget, which no other model can fix. Each built-in member already retries its own
// transient failures, so a member is given up on only once its retries are
// spent. A stream that fails after it has delivered output is not replayed
// on another model, since that would duplicate what the run already
// published.
func (c *config) modelChain(rev Revision) (saigetypes.Provider, []saigetypes.Provider, error) {
	rule, err := fallbackRule(rev.Spec)
	if err != nil {
		return nil, nil, err
	}
	refs := make([]ModelRef, 0, 1+len(rev.Spec.Fallbacks))
	refs = append(refs, ModelRef{Provider: rev.Spec.Provider, Model: rev.Spec.Model})
	refs = append(refs, rev.Spec.Fallbacks...)
	members := make([]saigetypes.Provider, 0, len(refs))
	for i, ref := range refs {
		newProvider, ok := c.provider(ref.Provider)
		if !ok {
			if i == 0 {
				return nil, nil, fmt.Errorf("%w: agent %q version %s names provider %q; this deployment has %v",
					ErrInvalid, rev.Spec.Name, rev.Version, ref.Provider, providerNames(c.providers))
			}
			return nil, nil, fmt.Errorf("%w: agent %q version %s fallback %d names provider %q; this deployment has %v",
				ErrInvalid, rev.Spec.Name, rev.Version, i, ref.Provider, providerNames(c.providers))
		}
		spec := rev.Spec
		spec.Provider, spec.Model, spec.Fallbacks, spec.FallbackOn = ref.Provider, ref.Model, nil, nil
		if i > 0 {
			// The effort was chosen for the primary; a fallback runs at
			// its own default rather than fail on a value it may not take.
			spec.Effort = ""
		}
		p, err := newProvider(spec)
		if err != nil {
			if i == 0 {
				return nil, nil, err
			}
			return nil, nil, fmt.Errorf("native: agent %q version %s fallback %d (%s %q): %w",
				rev.Spec.Name, rev.Version, i, ref.Provider, ref.Model, err)
		}
		if p == nil {
			return nil, nil, fmt.Errorf("%w: agent %q version %s: provider %q built no model", ErrInvalid, rev.Spec.Name, rev.Version, ref.Provider)
		}
		members = append(members, p)
	}
	if len(members) == 1 {
		return members[0], members, nil
	}
	chain, err := fallback.New(fallback.Config{Providers: members}, fallback.WithFallbackOn(rule))
	if err != nil {
		return nil, nil, fmt.Errorf("native: agent %q version %s: fallback: %w", rev.Spec.Name, rev.Version, err)
	}
	return chain, members, nil
}

// withRetry wraps a model adapter in saige's retry decorator. saige's adapters
// make one attempt per call (the Anthropic SDK's own retries are off), so this
// is the only retry layer: transient failures such as rate limits and
// overloads are retried with jittered backoff that honors Retry-After, and
// everything else surfaces at once.
func withRetry(p saigetypes.Provider) (saigetypes.Provider, error) {
	r, err := retry.New(p, retry.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("native: retry: %w", err)
	}
	return r, nil
}

// offlineProvider replays script, one entry per model call.
//
// It is saige's own agenttest.ScriptedProvider rather than something written
// here, so the offline path exercises the same provider seam the real adapters
// implement and a test cannot accidentally be given a more forgiving fake than
// production uses.
func offlineProvider(script [][]saigetypes.Delta) ProviderFunc {
	return func(spec Spec) (saigetypes.Provider, error) {
		responses := script
		if len(responses) == 0 {
			responses = [][]saigetypes.Delta{agenttest.TextResponse(defaultOfflineAnswer(spec))}
		}
		return &agenttest.ScriptedProvider{Responses: responses}, nil
	}
}

// defaultOfflineAnswer is what the offline provider says when no script was
// given. It names itself: an operator reading a Slack thread must never mistake
// it for a model's answer.
func defaultOfflineAnswer(spec Spec) string {
	return fmt.Sprintf("%s is running with the offline provider, so no model was called. "+
		"Set the agent's provider to %q or %q for real inference.",
		spec.Name, ProviderAnthropic, ProviderOpenAI)
}
