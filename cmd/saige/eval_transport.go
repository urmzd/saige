package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval/harness"
)

// evalHostKeys maps an OpenAI-compatible API host to the environment
// variables that hold a key for it, in lookup order. A key is only ever sent
// to the host it belongs to.
var evalHostKeys = map[string][]string{
	hostOpenAI:                          {envOpenAIKey},
	"generativelanguage.googleapis.com": {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
	"api.groq.com":                      {"GROQ_API_KEY"},
	"openrouter.ai":                     {"OPENROUTER_API_KEY"},
	"api.mistral.ai":                    {"MISTRAL_API_KEY"},
	"models.github.ai":                  {"GITHUB_TOKEN"},
	"models.inference.ai.azure.com":     {"GITHUB_TOKEN"},
}

// Key sources named in eval errors and plans.
const (
	envOpenAIKey     = "OPENAI_API_KEY"
	envEvalAPIKey    = "SAIGE_EVAL_API_KEY" //nolint:gosec // the name of a variable, not a credential
	envOpenAIBaseURL = "OPENAI_BASE_URL"
	keySourceFlag    = "--api-key"

	// hostOpenAI is the API host of OpenAI itself.
	hostOpenAI = "api.openai.com"
)

// evalProviderKeys names the key variable each saige provider reads.
var evalProviderKeys = map[string]string{
	providerOpenAI:    envOpenAIKey,
	providerAnthropic: "ANTHROPIC_API_KEY",
	providerGoogle:    "GOOGLE_API_KEY",
	providerOllama:    "",
}

// evalTransport describes the resolved model endpoint of an eval run. It
// never holds the key itself, only where the key comes from.
type evalTransport struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url,omitempty"`
	KeySource string `json:"key_source,omitempty"`
	KeySet    bool   `json:"key_set"`
}

// baseSourceManifest names a base URL read from the suite manifest. A
// manifest can come from the working directory, so its base URL is not
// trusted with keys the caller did not tie to that host.
const baseSourceManifest = "manifest"

// evalKeyEnvPrefix is the prefix of variables a manifest may name with
// api_key_env for any host the caller chose.
const evalKeyEnvPrefix = "SAIGE_EVAL_"

// evalBase resolves the OpenAI-compatible base URL and names where it came
// from: the caller's explicit value, then $SAIGE_EVAL_API_BASE, then
// $OPENAI_BASE_URL, then the public OpenAI API.
func evalBase(explicit string) (base, source string) {
	switch {
	case explicit != "":
		return explicit, "explicit"
	case os.Getenv("SAIGE_EVAL_API_BASE") != "":
		return os.Getenv("SAIGE_EVAL_API_BASE"), "SAIGE_EVAL_API_BASE"
	case os.Getenv(envOpenAIBaseURL) != "":
		return os.Getenv(envOpenAIBaseURL), envOpenAIBaseURL
	}
	return harness.DefaultAPIBase, "default"
}

// evalBaseFor resolves the base URL of cfg. A base URL from the manifest
// that --api-base did not override is reported as baseSourceManifest.
func evalBaseFor(cfg evalClientConfig) (base, source string) {
	if cfg.baseURL != "" && !cfg.baseURLFromFlag {
		return cfg.baseURL, baseSourceManifest
	}
	return evalBase(cfg.baseURL)
}

// evalAPIKey picks the key for an OpenAI-compatible base URL. The order is
// an explicit key, the variable the manifest names, $SAIGE_EVAL_API_KEY,
// $OPENAI_API_KEY when the base itself came from $OPENAI_BASE_URL, and then
// the variable that belongs to the base URL's host. A key for one vendor is
// never sent to another vendor's host, even when the manifest names it.
//
// A manifest may name only the host's own variables or a $SAIGE_EVAL_*
// variable, so it cannot point api_key_env at an unrelated secret. When the
// base URL itself came from the manifest, only --api-key and the host's own
// variables are sent: $SAIGE_EVAL_API_KEY and $SAIGE_EVAL_* variables go
// only to a host the caller chose. It returns the key (empty when none is
// found) and the name of its source, or the variable that should be set.
func evalAPIKey(base, baseSource, explicit, keyEnv string) (key, source string, err error) {
	if explicit != "" {
		return explicit, keySourceFlag, nil
	}
	host := evalHost(base)
	fromManifest := baseSource == baseSourceManifest
	if keyEnv != "" {
		if err := checkManifestKeyEnv(keyEnv, base, baseSource); err != nil {
			return "", keyEnv, err
		}
		if v := os.Getenv(keyEnv); v != "" {
			return v, keyEnv, nil
		}
		return "", keyEnv, fmt.Errorf("no API key: $%s, named by api_key_env, is not set", keyEnv)
	}
	if v := os.Getenv(envEvalAPIKey); v != "" && !fromManifest {
		return v, envEvalAPIKey, nil
	}
	if baseSource == envOpenAIBaseURL {
		if v := os.Getenv(envOpenAIKey); v != "" {
			return v, envOpenAIKey, nil
		}
	}
	envs := evalHostKeys[host]
	for _, env := range envs {
		if v := os.Getenv(env); v != "" {
			return v, env, nil
		}
	}
	if fromManifest {
		return "", envEvalAPIKey, fmt.Errorf("no API key for %s: the manifest set base_url, so $SAIGE_EVAL_API_KEY is not sent to it; pass --api-base to confirm the host, or pass --api-key", host)
	}
	if len(envs) > 0 {
		return "", envs[0], fmt.Errorf("no API key for %s: set $%s, $SAIGE_EVAL_API_KEY, or pass --api-key", host, strings.Join(envs, " or $"))
	}
	return "", envEvalAPIKey, fmt.Errorf("no API key for %s: set $SAIGE_EVAL_API_KEY, name a $%s* variable with api_key_env in the manifest, or pass --api-key", host, evalKeyEnvPrefix)
}

// checkManifestKeyEnv decides whether the variable a manifest names with
// api_key_env may be sent to base. The host's own variables always may. When
// the caller chose the base URL, a $SAIGE_EVAL_* variable may too, and so
// may $OPENAI_API_KEY for the base set by $OPENAI_BASE_URL, since both come
// from the caller's environment. Any other variable is refused, so a manifest
// cannot send an unrelated secret, or another vendor's key, as a bearer
// token.
func checkManifestKeyEnv(keyEnv, base, baseSource string) error {
	host := evalHost(base)
	if slices.Contains(evalHostKeys[host], keyEnv) {
		return nil
	}
	if baseSource == baseSourceManifest {
		return fmt.Errorf("api_key_env names $%s, which is not a key for %s, and the manifest set base_url; pass --api-base to confirm the host, or pass --api-key", keyEnv, host)
	}
	if keyEnv == envOpenAIKey && baseSource == envOpenAIBaseURL {
		return nil
	}
	if strings.HasPrefix(keyEnv, evalKeyEnvPrefix) {
		return nil
	}
	if isVendorKeyEnv(keyEnv) {
		return fmt.Errorf("api_key_env names $%s, which is not a key for %s; pass --api-key or set $SAIGE_EVAL_API_KEY for this host", keyEnv, host)
	}
	return fmt.Errorf("api_key_env names $%s, which is neither a key for %s nor a $%s* variable; rename it, pass --api-key, or set $SAIGE_EVAL_API_KEY", keyEnv, host, evalKeyEnvPrefix)
}

// isVendorKeyEnv reports whether a variable holds a key for a known vendor.
func isVendorKeyEnv(env string) bool {
	for _, envs := range evalHostKeys {
		if slices.Contains(envs, env) {
			return true
		}
	}
	for _, v := range evalProviderKeys {
		if v != "" && v == env {
			return true
		}
	}
	return false
}

// evalHost returns the lower-case host of a base URL, or the input when it
// does not parse.
func evalHost(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	return strings.ToLower(u.Hostname())
}

// evalClientConfig is everything needed to build the eval client.
type evalClientConfig struct {
	provider          string // harness.OpenAICompatible or a saige provider name
	model             string
	baseURL           string // flag or manifest base URL; empty resolves from env
	baseURLFromFlag   bool   // baseURL was set by --api-base on the command line
	apiKey            string // explicit --api-key
	apiKeyEnv         string // manifest api_key_env
	allowUnknownModel bool
}

// buildEvalClient resolves the transport and runs the offline preflight
// checks: the key is present and the model is in the catalog for the
// providers the catalog covers. With dryRun, a missing key is reported in
// the returned transport instead of failing, and no client is built.
func buildEvalClient(ctx context.Context, cfg evalClientConfig, dryRun bool) (*harness.Client, evalTransport, error) {
	if cfg.provider == "" || cfg.provider == harness.OpenAICompatible {
		return buildEvalHTTPClient(cfg, dryRun)
	}
	keyEnv, ok := evalProviderKeys[cfg.provider]
	if !ok {
		return nil, evalTransport{}, fmt.Errorf("unknown provider %q for eval (use %s, openai, anthropic, google or ollama)", cfg.provider, harness.OpenAICompatible)
	}
	flags := *persistentFlagVars
	flags.provider = &cfg.provider
	model := cfg.model
	flags.model = &model
	baseURL := cfg.baseURL
	if baseURL == "" {
		baseURL = *persistentFlagVars.baseURL
	}
	flags.baseURL = &baseURL
	model = flags.resolvedModel()
	tr := evalTransport{Provider: cfg.provider, Model: model, BaseURL: baseURL, KeySource: keyEnv, KeySet: keyEnv == "" || os.Getenv(keyEnv) != ""}
	// The openai provider sends $OPENAI_API_KEY to its base URL. A base URL
	// read from a manifest may only point at the OpenAI API; another host
	// must come from the command line, where the caller chose it.
	if cfg.provider == providerOpenAI && cfg.baseURL != "" && !cfg.baseURLFromFlag && evalHost(cfg.baseURL) != hostOpenAI {
		return nil, tr, fmt.Errorf("manifest base_url %s is not the OpenAI API, so $%s is not sent to it; pass --api-base to confirm the host, or use provider %q with $SAIGE_EVAL_API_KEY", cfg.baseURL, envOpenAIKey, harness.OpenAICompatible)
	}
	if err := checkEvalModel(cfg.provider, model, cfg.allowUnknownModel); err != nil {
		return nil, tr, err
	}
	if dryRun {
		return nil, tr, nil
	}
	p, err := buildProvider(ctx, &flags, false)
	if err != nil {
		return nil, tr, err
	}
	// Match the HTTP transport's retry budget: 6 attempts, honoring
	// Retry-After up to the harness cap.
	rc := retry.DefaultConfig()
	rc.MaxAttempts = 6
	rc.MaxDelay = 16 * time.Second
	rc.MaxRetryAfter = harness.MaxRetryAfter
	return harness.NewProviderClient(retry.New(p, rc)), tr, nil
}

func buildEvalHTTPClient(cfg evalClientConfig, dryRun bool) (*harness.Client, evalTransport, error) {
	base, baseSource := evalBaseFor(cfg)
	model := cfg.model
	if model == "" {
		model = harness.DefaultModel
	}
	key, keySource, keyErr := evalAPIKey(base, baseSource, cfg.apiKey, cfg.apiKeyEnv)
	tr := evalTransport{Provider: harness.OpenAICompatible, Model: model, BaseURL: base, KeySource: keySource, KeySet: key != ""}
	// Only the OpenAI API serves catalog model names; other hosts serve
	// their own.
	if evalHost(base) == hostOpenAI {
		if err := checkEvalModel(providerOpenAI, model, cfg.allowUnknownModel); err != nil {
			return nil, tr, err
		}
	}
	if dryRun {
		return nil, tr, nil
	}
	if keyErr != nil {
		return nil, tr, keyErr
	}
	return harness.NewClient(base, key, model), tr, nil
}

// checkEvalModel refuses a model that matches no catalog family for a
// provider whose model names the catalog declares, unless allowed. A model
// that only matches a family by prefix runs with a note, since dated or
// suffixed variants of a declared model are routine.
func checkEvalModel(provider, model string, allow bool) error {
	if allow || provider == providerOllama {
		return nil
	}
	entry, ok := catalog.Describe(types.ProviderName(provider), model)
	if !ok {
		return fmt.Errorf("model %q is not in the %s catalog; check the name, or pass --allow-unknown-model to run it anyway", model, provider)
	}
	if !strings.EqualFold(string(entry.Prefix), model) {
		fmt.Fprintf(os.Stderr, "note: model %q is not declared in the %s catalog; using the capabilities of family %q\n", model, provider, entry.Prefix)
	}
	return nil
}
