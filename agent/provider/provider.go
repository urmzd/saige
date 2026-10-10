// Package provider builds a provider adapter from a configuration or from a
// model name alone.
//
// Each adapter package has its own constructor and its own option names for
// the same controls: temperature is WithTemperature on Anthropic and OpenAI, a
// GenerationConfig field on Google, and an options map entry on Ollama. Build
// takes one provider-neutral types.RequestOptions and applies it through each
// adapter's own options, then validates the result against the catalog, so a
// control the target model does not accept fails here rather than on the
// first request. A control an adapter has no way to send fails the same way;
// it is never dropped.
//
// When Config.Provider is empty the provider is inferred from the model name
// through the catalog, with "provider/model" accepted as an explicit form.
// Credentials and the Ollama host are read from the environment when the
// configuration leaves them empty.
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go/option"
	openaisdk "github.com/openai/openai-go/v3/option"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/google"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// Provider names accepted by Config.Provider. They match each adapter's Name.
const (
	Anthropic types.ProviderName = "anthropic"
	OpenAI    types.ProviderName = "openai"
	Google    types.ProviderName = "google"
	Ollama    types.ProviderName = "ollama"
)

// DefaultOllamaHost is used when neither Config.BaseURL nor OLLAMA_HOST is set.
const DefaultOllamaHost = "http://localhost:11434"

// ErrUnknownProvider reports a provider name Build does not construct, or a
// model name from which no provider can be inferred.
var ErrUnknownProvider = errors.New("unknown provider")

// APIKeyEnv lists the environment variables read for each provider's key, in
// order of preference.
var APIKeyEnv = map[types.ProviderName][]string{
	Anthropic: {"ANTHROPIC_API_KEY"},
	OpenAI:    {"OPENAI_API_KEY"},
	Google:    {"GOOGLE_API_KEY", "GEMINI_API_KEY"},
}

// Config describes one provider configuration.
type Config struct {
	// Provider is one of the names above. Empty infers it from Model.
	Provider types.ProviderName
	// Model is the provider's model identifier. A "provider/" prefix naming a
	// known provider is accepted and removed.
	Model types.ModelID
	// APIKey overrides the environment. Ollama needs none and rejects one,
	// since its adapter sends no key. OpenAI with a
	// BaseURL may also run without one, for compatible servers that do not
	// check keys.
	APIKey string
	// BaseURL targets a gateway or compatible server for Anthropic and
	// OpenAI, and is the host for Ollama. Google does not accept one.
	BaseURL string
	// HTTPClient replaces the transport for every adapter. On Vertex AI a
	// client given here must authenticate its requests itself; without one,
	// Application Default Credentials are attached.
	HTTPClient *http.Client
	// Options are applied through the adapter's own options.
	Options types.RequestOptions
	// Dials are global dials, compiled per request against the model (see
	// types.ResolveDials). The model's own dial defaults apply above them.
	// A raw option in Options that sets the same parameter wins.
	Dials types.Dials
	// DialLayers are dial layers already resolved, such as a catalog preset
	// entry's. When set, Dials and the model's dial defaults are not added
	// again.
	DialLayers []types.DialLayer
	// DialPolicy sets how dials the model cannot honor are handled. Nil
	// uses each dial's class.
	DialPolicy *types.DialPolicy
	// ServerTools enables provider-executed tools on adapters that send them
	// (Anthropic and Google).
	ServerTools []types.ServerTool
	// PromptCache configures provider-side prompt caching. Nil or mode "off"
	// sends no cache controls. Markers map to Anthropic's cache policy and
	// automatic maps to OpenAI's prompt cache key and retention; Google and
	// Ollama accept only off.
	PromptCache *PromptCache
	// Vertex serves a Google model through Vertex AI instead of the Gemini
	// API. It is also selected by GOOGLE_GENAI_USE_VERTEXAI=true. Empty
	// fields default from GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION,
	// and the location then defaults to "global". Vertex authenticates with
	// Application Default Credentials, so no API key is read.
	Vertex *Vertex
	// Getenv reads the environment. Nil uses os.Getenv.
	Getenv func(string) string
	// Conversion is how parts the model cannot take natively are fitted to
	// it. The zero value rejects them. The modality dial of Dials and
	// DialLayers applies above Conversion.Dial, and an agent's policy
	// (agent.WithConversion) above both.
	Conversion types.ConversionPolicy
}

// Vertex names the Google Cloud project and location that serve a Google
// model through Vertex AI.
type Vertex struct {
	Project  string
	Location string
}

// Vertex environment variables, as the Gen AI SDK reads them.
const (
	EnvUseVertex       = "GOOGLE_GENAI_USE_VERTEXAI"
	EnvCloudProject    = "GOOGLE_CLOUD_PROJECT"
	EnvCloudLocation   = "GOOGLE_CLOUD_LOCATION"
	DefaultVertexPlace = "global"
)

// VertexEnabled reports whether the environment selects Vertex AI for Google
// models (GOOGLE_GENAI_USE_VERTEXAI set to true or 1).
func VertexEnabled(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	v := strings.ToLower(strings.TrimSpace(getenv(EnvUseVertex)))
	return v == "true" || v == "1"
}

// ResolveVertex returns the Vertex settings for a Google configuration, or
// nil when it targets the Gemini API. Explicit fields win over the
// environment, and the location defaults to "global".
func ResolveVertex(v *Vertex, getenv func(string) string) *Vertex {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v == nil && !VertexEnabled(getenv) {
		return nil
	}
	out := Vertex{}
	if v != nil {
		out = *v
	}
	if out.Project == "" {
		out.Project = strings.TrimSpace(getenv(EnvCloudProject))
	}
	if out.Location == "" {
		out.Location = strings.TrimSpace(getenv(EnvCloudLocation))
	}
	if out.Location == "" {
		out.Location = DefaultVertexPlace
	}
	return &out
}

// PromptCache is a provider-neutral prompt cache configuration.
type PromptCache struct {
	// Mode is "off", "markers" or "automatic" (catalog.PromptCache*).
	Mode string
	// TTL is the marker lifetime, "5m" or "1h" (markers).
	TTL string
	// Tools, System and Conversation choose where markers go (markers).
	Tools, System, Conversation bool
	// Retention is "", "in_memory" or "24h" (automatic).
	Retention string
	// Key groups requests that share a prefix (automatic).
	Key string
}

// FromModel builds a provider for a model name, inferring the provider and
// reading credentials from the environment.
func FromModel(ctx context.Context, model string) (types.Provider, error) {
	return Build(ctx, Config{Model: types.ModelID(model)})
}

// Infer returns the provider and bare model name for a model identifier. An
// explicit "provider/model" form wins; otherwise the catalog's longest
// matching family decides, and an unmatched name with an Ollama-style ":tag"
// is taken as a local model.
func Infer(model string) (provider types.ProviderName, bare string, err error) {
	if p, rest, ok := strings.Cut(model, "/"); ok && rest != "" {
		if name := knownProvider(p); name != "" {
			return name, rest, nil
		}
	}
	if p, ok := catalog.InferProvider(model); ok && knownProvider(string(p)) != "" {
		return p, model, nil
	}
	if strings.Contains(model, ":") {
		return Ollama, model, nil
	}
	return "", model, fmt.Errorf("%w: cannot infer a provider for model %q; set Config.Provider", ErrUnknownProvider, model)
}

func knownProvider(name string) types.ProviderName {
	switch n := types.ProviderName(strings.ToLower(strings.TrimSpace(name))); n {
	case Anthropic, OpenAI, Google, Ollama:
		return n
	}
	return ""
}

// Build constructs and validates the adapter cfg describes, behind a
// conversion decorator (convert.Provider) that fits each request's parts to
// the model's offering with cfg.Conversion and the modality dial. Use
// wrapper.As to reach the adapter itself.
func Build(ctx context.Context, cfg Config) (types.Provider, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	name, model := knownProvider(string(cfg.Provider)), string(cfg.Model)
	switch {
	case cfg.Provider != "" && name == "":
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, cfg.Provider)
	case name == "":
		var err error
		if name, model, err = Infer(model); err != nil {
			return nil, err
		}
	default:
		if p, rest, ok := strings.Cut(model, "/"); ok && knownProvider(p) == name && rest != "" {
			model = rest
		}
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("provider %s: a model is required", name)
	}

	caps := catalog.MustLookup(name, model)
	key, vertex, err := credentialsFor(caps, name, model, cfg, getenv)
	if err != nil {
		return nil, err
	}
	if name == Ollama && cfg.APIKey != "" {
		return nil, caps.OptionError("api_key", "not sent by the ollama adapter")
	}
	if len(cfg.ServerTools) > 0 && catalog.ExpressibleServerTools(name) != nil {
		return nil, caps.OptionError("server_tools", "not sent by the "+string(name)+" adapter")
	}
	if err := checkExpressible(caps, name, cfg); err != nil {
		return nil, err
	}
	if cfg, err = withDials(cfg, caps); err != nil {
		return nil, err
	}
	// The modality dial is spent by the conversion decorator; the adapter
	// gets the dials that compile to options.
	var modality []types.DialLayer
	cfg.DialLayers, modality = splitModality(cfg.DialLayers)

	var p types.Provider
	switch name {
	case Anthropic:
		p, err = buildAnthropic(cfg, model, key, caps)
	case OpenAI:
		p, err = buildOpenAI(cfg, model, key, caps)
	case Google:
		p, err = buildGoogle(ctx, cfg, model, key, vertex, caps)
	case Ollama:
		p, err = buildOllama(cfg, model, getenv, caps)
	}
	if err != nil {
		return nil, err
	}
	if v, ok := p.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	return convert.New(p, convert.Config{Policy: cfg.Conversion, Layers: modality})
}

// splitModality separates the modality dial from the layers an adapter
// compiles.
func splitModality(layers []types.DialLayer) (rest, modality []types.DialLayer) {
	for _, l := range layers {
		if l.Dials.Modality != nil {
			modality = append(modality, types.DialLayer{Scope: l.Scope, Dials: types.Dials{Modality: l.Dials.Modality}}.Clone())
			l = l.Clone()
			l.Dials.Modality = nil
			if l.Dials.IsZero() && l.Hold == nil {
				continue
			}
		}
		rest = append(rest, l)
	}
	return rest, modality
}

// credentialsFor resolves how the adapter authenticates: an API key, or for
// Google on Vertex AI, the project and location used with Application
// Default Credentials.
func credentialsFor(caps types.ModelCapabilities, name types.ProviderName, model string, cfg Config, getenv func(string) string) (string, *Vertex, error) {
	var vertex *Vertex
	switch {
	case name == Google:
		vertex = ResolveVertex(cfg.Vertex, getenv)
	case cfg.Vertex != nil:
		return "", nil, caps.OptionError("vertex", "applies only to google")
	}
	if vertex != nil {
		if cfg.APIKey != "" {
			return "", nil, caps.OptionError("api_key", "vertex authenticates with Application Default Credentials, not an API key")
		}
		if vertex.Project == "" {
			return "", nil, &types.ProviderError{Provider: string(name), Model: model, Kind: types.ErrorKindAuth,
				Err: fmt.Errorf("%w: vertex needs a project: set Config.Vertex.Project or %s", types.ErrAuth, EnvCloudProject)}
		}
		return "", vertex, nil
	}
	key := cfg.APIKey
	for _, env := range APIKeyEnv[name] {
		if key != "" {
			break
		}
		key = getenv(env)
	}
	if key == "" && name != Ollama && (name != OpenAI || cfg.BaseURL == "") {
		return "", nil, &types.ProviderError{Provider: string(name), Model: model, Kind: types.ErrorKindAuth,
			Err: fmt.Errorf("%w: no API key: set Config.APIKey or %s", types.ErrAuth, strings.Join(APIKeyEnv[name], " or "))}
	}
	return key, nil, nil
}

// unsupported rejects a control the adapter has no option for.
func unsupported(caps types.ModelCapabilities, option string) error {
	return caps.OptionError(option, "not expressible by the "+caps.Provider+" adapter")
}

// checkExpressible applies the catalog's shared expressibility table, so a
// control the adapter cannot send fails the same way here and in catalog
// validation.
func checkExpressible(caps types.ModelCapabilities, name types.ProviderName, cfg Config) error {
	var ee *catalog.ExpressError
	err := catalog.Expressible(name, cfg.Options)
	if err == nil && cfg.PromptCache != nil {
		err = catalog.ExpressiblePromptCache(name, cfg.PromptCache.Mode)
	}
	if errors.As(err, &ee) {
		return caps.OptionError(ee.Option, ee.Reason)
	}
	if err == nil && cfg.PromptCache != nil && cfg.PromptCache.Retention != "" && caps.Offering != nil {
		// A model that keeps prompt caches for a fixed time rejects any
		// other retention; refuse it here rather than on the first call.
		err = caps.Offering.AcceptsValue(types.ParamPromptCacheRetention, catalog.NormalizeRetention(cfg.PromptCache.Retention))
	}
	return err
}

func buildAnthropic(cfg Config, model, key string, caps types.ModelCapabilities) (types.Provider, error) {
	o := cfg.Options
	var opts []anthropic.Option
	if cfg.BaseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, anthropic.WithRequestOptions(anthropicsdk.WithHTTPClient(cfg.HTTPClient)))
	}
	if o.Temperature != nil {
		opts = append(opts, anthropic.WithTemperature(*o.Temperature))
	}
	if o.TopP != nil {
		opts = append(opts, anthropic.WithTopP(*o.TopP))
	}
	if o.TopK != nil {
		k, err := integer(caps, "top_k", *o.TopK)
		if err != nil {
			return nil, err
		}
		opts = append(opts, anthropic.WithTopK(k))
	}
	if len(o.StopSequences) > 0 {
		opts = append(opts, anthropic.WithStopSequences(o.StopSequences...))
	}
	if o.MaxOutputTokens != nil {
		opts = append(opts, anthropic.WithMaxTokens(*o.MaxOutputTokens))
	}
	if o.ParallelTools != nil {
		opts = append(opts, anthropic.WithParallelToolCalls(*o.ParallelTools))
	}
	if o.ReasoningBudget != nil {
		opts = append(opts, anthropic.WithThinking(*o.ReasoningBudget))
	}
	if o.ReasoningEffort != nil {
		opts = append(opts, anthropic.WithReasoningEffort(*o.ReasoningEffort))
	}
	if o.ToolChoice != nil {
		opts = append(opts, anthropic.WithToolChoice(*o.ToolChoice))
	}
	if len(cfg.ServerTools) > 0 {
		opts = append(opts, anthropic.WithServerTools(cfg.ServerTools...))
	}
	if pc := cfg.PromptCache; pc != nil && pc.Mode == catalog.PromptCacheMarkers {
		opts = append(opts, anthropic.WithPromptCachePolicy(anthropic.PromptCachePolicy{
			TTL: pc.TTL, Tools: pc.Tools, System: pc.System, Conversation: pc.Conversation}))
	}
	if len(cfg.DialLayers) > 0 {
		opts = append(opts, anthropic.WithDials(cfg.DialLayers...))
	}
	if cfg.DialPolicy != nil {
		opts = append(opts, anthropic.WithDialPolicy(*cfg.DialPolicy))
	}
	return anthropic.NewAdapter(key, model, opts...), nil
}

func buildOpenAI(cfg Config, model, key string, caps types.ModelCapabilities) (types.Provider, error) {
	o := cfg.Options
	var opts []openai.Option
	if cfg.BaseURL != "" {
		opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, openai.WithRequestOptions(openaisdk.WithHTTPClient(cfg.HTTPClient)))
	}
	if o.Temperature != nil {
		opts = append(opts, openai.WithTemperature(*o.Temperature))
	}
	if o.TopP != nil {
		opts = append(opts, openai.WithTopP(*o.TopP))
	}
	if o.Seed != nil {
		opts = append(opts, openai.WithSeed(*o.Seed))
	}
	if len(o.StopSequences) > 0 {
		opts = append(opts, openai.WithStopSequences(o.StopSequences...))
	}
	if o.MaxOutputTokens != nil {
		opts = append(opts, openai.WithMaxTokens(*o.MaxOutputTokens))
	}
	if o.FrequencyPenalty != nil {
		opts = append(opts, openai.WithFrequencyPenalty(*o.FrequencyPenalty))
	}
	if o.PresencePenalty != nil {
		opts = append(opts, openai.WithPresencePenalty(*o.PresencePenalty))
	}
	if o.ReasoningEffort != nil {
		opts = append(opts, openai.WithReasoningEffort(*o.ReasoningEffort))
	}
	if o.ParallelTools != nil {
		opts = append(opts, openai.WithParallelToolCalls(*o.ParallelTools))
	}
	if o.ToolChoice != nil {
		opts = append(opts, openai.WithToolChoice(*o.ToolChoice))
	}
	if pc := cfg.PromptCache; pc != nil && pc.Mode == catalog.PromptCacheAutomatic {
		opts = append(opts, openai.WithPromptCache(pc.Key, pc.Retention))
	}
	if len(cfg.DialLayers) > 0 {
		opts = append(opts, openai.WithDials(cfg.DialLayers...))
	}
	if cfg.DialPolicy != nil {
		opts = append(opts, openai.WithDialPolicy(*cfg.DialPolicy))
	}
	switch caps.ChatCompletionsTools {
	case types.ChatToolsResponsesOnly:
		// The model calls tools only through the Responses API, so it is
		// served there for every request, not only those with tools.
		return openai.NewResponsesAdapter(key, model, opts...), nil
	case types.ChatToolsNoReasoning:
		// Chat Completions would turn a reasoning dial off whenever tools
		// are offered; the Responses API keeps it. A configuration the
		// Responses API cannot send stays on Chat Completions.
		if r := mergedDials(cfg.DialLayers).Reasoning; r != nil && r.Mode != types.ReasoningOff {
			if ra := openai.NewResponsesAdapter(key, model, opts...); ra.Validate() == nil {
				return ra, nil
			}
		}
	}
	return openai.NewAdapter(key, model, opts...), nil
}

func buildGoogle(ctx context.Context, cfg Config, model, key string, vertex *Vertex, caps types.ModelCapabilities) (types.Provider, error) {
	o := cfg.Options
	if cfg.BaseURL != "" {
		return nil, unsupported(caps, "base_url")
	}
	var g google.GenerationConfig
	f32 := func(p *float64) *float32 {
		if p == nil {
			return nil
		}
		v := float32(*p)
		return &v
	}
	g.Temperature, g.TopP, g.TopK = f32(o.Temperature), f32(o.TopP), f32(o.TopK)
	g.FrequencyPenalty, g.PresencePenalty = f32(o.FrequencyPenalty), f32(o.PresencePenalty)
	g.StopSequences = o.StopSequences
	if o.Seed != nil {
		n, err := int32Of(caps, "seed", *o.Seed)
		if err != nil {
			return nil, err
		}
		g.Seed = &n
	}
	if o.MaxOutputTokens != nil {
		n, err := int32Of(caps, "max_output_tokens", *o.MaxOutputTokens)
		if err != nil {
			return nil, err
		}
		g.MaxOutputTokens = n
	}
	opts := []google.Option{google.WithGenerationConfig(g)}
	if vertex != nil {
		opts = append(opts, google.WithVertex(vertex.Project, vertex.Location))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, google.WithHTTPClient(cfg.HTTPClient))
	}
	switch {
	case o.ReasoningBudget != nil:
		n, err := int32Of(caps, "reasoning_budget", *o.ReasoningBudget)
		if err != nil {
			return nil, err
		}
		opts = append(opts, google.WithThinkingBudget(n))
	case o.ReasoningEffort != nil:
		opts = append(opts, google.WithThinkingLevel(genai.ThinkingLevel(strings.ToUpper(*o.ReasoningEffort))))
	case o.ReasoningEnabled != nil:
		opts = append(opts, google.WithoutThinking())
	}
	if o.ToolChoice != nil {
		opts = append(opts, google.WithToolChoice(*o.ToolChoice))
	}
	if len(cfg.ServerTools) > 0 {
		opts = append(opts, google.WithServerTools(cfg.ServerTools...))
	}
	if len(cfg.DialLayers) > 0 {
		opts = append(opts, google.WithDials(cfg.DialLayers...))
	}
	if cfg.DialPolicy != nil {
		opts = append(opts, google.WithDialPolicy(*cfg.DialPolicy))
	}
	// Two reasoning controls cannot both reach the adapter, so check the
	// combination here, where the caller's options are still whole.
	if err := caps.ValidateOptions(o); err != nil {
		return nil, err
	}
	return google.NewAdapter(ctx, key, model, opts...)
}

func buildOllama(cfg Config, model string, getenv func(string) string, caps types.ModelCapabilities) (types.Provider, error) {
	o := cfg.Options
	host := cfg.BaseURL
	if host == "" {
		host = getenv("OLLAMA_HOST")
	}
	if host == "" {
		host = DefaultOllamaHost
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	var clientOpts []ollama.Option
	if cfg.HTTPClient != nil {
		clientOpts = append(clientOpts, ollama.WithHTTPClient(cfg.HTTPClient))
	}
	if o.ReasoningEnabled != nil {
		clientOpts = append(clientOpts, ollama.WithThink(*o.ReasoningEnabled))
	}
	options := map[string]any{}
	if o.Temperature != nil {
		options["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		options["top_p"] = *o.TopP
	}
	if o.TopK != nil {
		k, err := integer(caps, "top_k", *o.TopK)
		if err != nil {
			return nil, err
		}
		options["top_k"] = k
	}
	if o.Seed != nil {
		options["seed"] = *o.Seed
	}
	if o.MaxOutputTokens != nil {
		options["num_predict"] = *o.MaxOutputTokens
	}
	if len(o.StopSequences) > 0 {
		options["stop"] = o.StopSequences
	}
	if len(options) > 0 {
		clientOpts = append(clientOpts, ollama.WithChatOptions(options))
	}
	var adapterOpts []ollama.AdapterOption
	if o.ToolChoice != nil {
		adapterOpts = append(adapterOpts, ollama.WithToolChoice(*o.ToolChoice))
	}
	if len(cfg.DialLayers) > 0 {
		adapterOpts = append(adapterOpts, ollama.WithDials(cfg.DialLayers...))
	}
	if cfg.DialPolicy != nil {
		adapterOpts = append(adapterOpts, ollama.WithDialPolicy(*cfg.DialPolicy))
	}
	return ollama.NewAdapter(ollama.NewClient(host, model, "", clientOpts...), adapterOpts...), nil
}

// integer converts a whole-number control carried as float64.
func integer(caps types.ModelCapabilities, option string, v float64) (int64, error) {
	if v != math.Trunc(v) || math.IsInf(v, 0) || math.IsNaN(v) || v > math.MaxInt64 || v < math.MinInt64 {
		return 0, caps.OptionError(option, "must be a whole number")
	}
	return int64(v), nil
}

// int32Of narrows a control to the int32 the Gemini API takes.
func int32Of(caps types.ModelCapabilities, option string, v int64) (int32, error) {
	if v > math.MaxInt32 || v < math.MinInt32 {
		return 0, caps.OptionError(option, "out of range")
	}
	return int32(v), nil
}
