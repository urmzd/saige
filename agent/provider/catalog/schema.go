package catalog

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// SchemaVersion is the major version of the catalog file format this package
// reads and writes. A file with another major version is rejected rather
// than half understood.
const SchemaVersion = 1

// Catalog is one catalog document: model rows, templates they extend,
// per-provider baselines, and named presets. It is the JSON file format and
// the in-memory value hosts pass around. Model rows reach adapters through
// Install; presets never become global state.
type Catalog struct {
	// Schema is an editor hint and is ignored.
	Schema string `json:"$schema,omitempty"`
	// Version is the major schema version. Required.
	Version int `json:"version"`
	// Revision is a free-form label for this catalog, recorded in route
	// events, traces and eval provenance. Merging layers joins their
	// revisions with "+".
	Revision string `json:"revision,omitempty"`
	// InheritDefault false starts this layer from an empty catalog instead of
	// merging onto the layers below it. Nil means true.
	InheritDefault *bool `json:"inherit_default,omitempty"`
	// Templates are partial rows reachable only through Extends.
	Templates map[string]ModelSpec `json:"templates,omitempty"`
	// Models are the rows Lookup matches, keyed by (Provider, Prefix).
	Models []ModelSpec `json:"models,omitempty"`
	// Baselines are the conservative per-provider fallbacks.
	Baselines map[string]ModelSpec `json:"baselines,omitempty"`
	// Presets are named, complete provider configurations.
	Presets map[string]PresetSpec `json:"presets,omitempty"`
	// DefaultPreset is used by the CLI when neither a preset nor a model is
	// named.
	DefaultPreset string `json:"default_preset,omitempty"`

	// deletedPresets lists the presets a layer removes with null.
	deletedPresets []string
}

// ModelSpec is one model row, template, or baseline. Every field is optional
// in a template or an overlay; a resolved row inherits what it leaves unset
// from the template it extends.
type ModelSpec struct {
	Provider string `json:"provider,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	// Extends names a template. Chains are allowed up to four deep.
	Extends      string `json:"extends,omitempty"`
	Tier         Tier   `json:"tier,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// ChatCompletionsTools is "any", "no_reasoning" (tools on OpenAI Chat
	// Completions need reasoning effort none) or "responses_only" (tools
	// need the Responses API). Empty inherits.
	ChatCompletionsTools string `json:"chat_completions_tools,omitempty"`
	// Capabilities replaces the inherited list when non-empty.
	// AddCapabilities and RemoveCapabilities then edit it.
	Capabilities       []types.Capability `json:"capabilities,omitempty"`
	AddCapabilities    []types.Capability `json:"add_capabilities,omitempty"`
	RemoveCapabilities []types.Capability `json:"remove_capabilities,omitempty"`
	Limits             *LimitsSpec        `json:"limits,omitempty"`
	Reasoning          *ReasoningSpec     `json:"reasoning,omitempty"`
	// StructuredOutput is "", "native" or "tool_call". Nil inherits.
	StructuredOutput *types.StructuredOutputMode `json:"structured_output,omitempty"`
	// Media replaces the inherited media list when set.
	Media          []types.MediaType            `json:"media,omitempty"`
	ServerTools    []types.ServerToolKind       `json:"server_tools,omitempty"`
	ServerToolFees map[types.ServerToolKind]Fee `json:"server_tool_fees,omitempty"`
	Pricing        *PricingSpec                 `json:"pricing,omitempty"`
	// Defaults are model-level option defaults, the lowest declared layer of
	// a preset entry's options.
	Defaults *OptionsSpec `json:"defaults,omitempty"`
	Notes    []string     `json:"notes,omitempty"`
	// Replace makes an overlay row replace the base row whole instead of
	// patching it. Delete removes the base row.
	Replace bool `json:"$replace,omitempty"`
	Delete  bool `json:"$delete,omitempty"`

	// raw is the row as written, kept so a merge can tell an absent key
	// from an explicit null.
	raw json.RawMessage
}

// LimitsSpec declares token limits. Zero means undeclared.
type LimitsSpec struct {
	ContextWindow          *int `json:"context_window,omitempty"`
	MaxOutputTokens        *int `json:"max_output_tokens,omitempty"`
	DefaultMaxOutputTokens *int `json:"default_max_output_tokens,omitempty"`
}

// ReasoningSpec declares how a family sizes and defaults its reasoning.
type ReasoningSpec struct {
	Efforts                     []string           `json:"efforts,omitempty"`
	DefaultEffort               *string            `json:"default_effort,omitempty"`
	Required                    *bool              `json:"required,omitempty"`
	DefaultEnabled              *bool              `json:"default_enabled,omitempty"`
	MinBudget                   *int               `json:"min_budget,omitempty"`
	MaxBudget                   *int               `json:"max_budget,omitempty"`
	DynamicBudget               *bool              `json:"dynamic_budget,omitempty"`
	ZeroBudget                  *bool              `json:"zero_budget,omitempty"`
	SamplingRequiresNoReasoning []types.Capability `json:"sampling_requires_no_reasoning,omitempty"`
	// ForcedToolChoice false declares that the API rejects a required or
	// named tool choice for the model, as some always-thinking models do.
	// Nil or true leaves forcing to CapToolChoice.
	ForcedToolChoice *bool `json:"forced_tool_choice,omitempty"`
}

// PricingSpec is the JSON form of types.Pricing. Rates are per million tokens.
type PricingSpec struct {
	Currency           string  `json:"currency,omitempty"`
	InputPerMTok       float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok      float64 `json:"output_per_mtok,omitempty"`
	CachedInputPerMTok float64 `json:"cached_input_per_mtok,omitempty"`
	CacheWritePerMTok  float64 `json:"cache_write_per_mtok,omitempty"`
	PerRequest         float64 `json:"per_request,omitempty"`
	Free               bool    `json:"free,omitempty"`
	AsOf               string  `json:"as_of,omitempty"`
	Source             string  `json:"source,omitempty"`
}

func (p PricingSpec) pricing() types.Pricing {
	return types.Pricing{Currency: p.Currency, InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok,
		CachedInputPerMTok: p.CachedInputPerMTok, CacheWritePerMTok: p.CacheWritePerMTok,
		PerRequest: p.PerRequest, Free: p.Free, AsOf: p.AsOf, Source: p.Source}
}

func pricingSpec(p types.Pricing) *PricingSpec {
	if p == (types.Pricing{}) {
		return nil
	}
	return &PricingSpec{Currency: p.Currency, InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok,
		CachedInputPerMTok: p.CachedInputPerMTok, CacheWritePerMTok: p.CacheWritePerMTok,
		PerRequest: p.PerRequest, Free: p.Free, AsOf: p.AsOf, Source: p.Source}
}

// OptionsSpec is the options object used everywhere options are declared:
// model defaults, preset options and chain entry overrides. JSON null is not
// accepted inside it; remove an inherited option with a chain entry's unset.
type OptionsSpec struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *float64 `json:"top_k,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	MaxOutputTokens  *int64   `json:"max_output_tokens,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	ParallelTools    *bool    `json:"parallel_tools,omitempty"`
	// Reasoning sets exactly one of enabled, effort or budget.
	Reasoning *ReasoningOption `json:"reasoning,omitempty"`
	// ToolChoice is "auto" or "none". Forced choices belong on the preset,
	// where the agent applies them to one turn.
	ToolChoice  string           `json:"tool_choice,omitempty"`
	PromptCache *PromptCacheSpec `json:"prompt_cache,omitempty"`
	ServerTools []ServerToolSpec `json:"server_tools,omitempty"`
}

// ReasoningOption is the one reasoning control of an options object.
type ReasoningOption struct {
	Enabled *bool   `json:"enabled,omitempty"`
	Effort  *string `json:"effort,omitempty"`
	Budget  *int64  `json:"budget,omitempty"`
}

// PromptCacheSpec configures provider-side prompt caching. Mode "markers"
// uses TTL, Tools, System and Conversation (Anthropic); mode "automatic" uses
// Retention and Key (OpenAI); mode "off" sends nothing.
type PromptCacheSpec struct {
	Mode         string `json:"mode"`
	TTL          string `json:"ttl,omitempty"`
	Tools        bool   `json:"tools,omitempty"`
	System       bool   `json:"system,omitempty"`
	Conversation bool   `json:"conversation,omitempty"`
	Retention    string `json:"retention,omitempty"`
	Key          string `json:"key,omitempty"`
}

// ServerToolSpec is the JSON form of types.ServerTool. A remote MCP server's
// authorization token is not part of the file format.
type ServerToolSpec struct {
	Kind           types.ServerToolKind `json:"kind"`
	MaxUses        int                  `json:"max_uses,omitempty"`
	AllowedDomains []string             `json:"allowed_domains,omitempty"`
	BlockedDomains []string             `json:"blocked_domains,omitempty"`
	UserLocation   string               `json:"user_location,omitempty"`
	MCPServer      *MCPServerSpec       `json:"mcp_server,omitempty"`
}

// MCPServerSpec names a remote MCP server the provider connects to.
type MCPServerSpec struct {
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	RequireApproval bool     `json:"require_approval,omitempty"`
}

func (s ServerToolSpec) serverTool() types.ServerTool {
	st := types.ServerTool{Kind: s.Kind, MaxUses: s.MaxUses, UserLocation: s.UserLocation,
		AllowedDomains: append([]string(nil), s.AllowedDomains...), BlockedDomains: append([]string(nil), s.BlockedDomains...)}
	if s.MCPServer != nil {
		st.MCPServer = &types.RemoteMCPServer{Name: s.MCPServer.Name, URL: s.MCPServer.URL,
			AllowedTools: append([]string(nil), s.MCPServer.AllowedTools...), RequireApproval: s.MCPServer.RequireApproval}
	}
	return st
}

// PresetSpec is a named configuration: an ordered chain of complete provider
// entries plus the agent-level defaults that go with them.
type PresetSpec struct {
	Description string `json:"description,omitempty"`
	// Extends copies another preset, then overrides its top-level keys. A
	// chain set here replaces the parent's chain whole.
	Extends string `json:"extends,omitempty"`
	// Options are inherited by every chain entry that does not opt out.
	Options *OptionsSpec `json:"options,omitempty"`
	// ToolChoice is the agent default: auto, none, required or named:<tool>.
	ToolChoice string `json:"tool_choice,omitempty"`
	// OutputMode is auto, native, tool or prompt.
	OutputMode string `json:"output_mode,omitempty"`
	// LLMTimeout bounds one provider call, as a Go duration string.
	LLMTimeout Duration `json:"llm_timeout,omitzero"`
	// Retry is the default retry policy of every entry.
	Retry   *RetrySpec   `json:"retry,omitempty"`
	Routing *RoutingSpec `json:"routing,omitempty"`
	// RequireDeclared requires every entry's model to be an exact row.
	RequireDeclared *bool `json:"require_declared,omitempty"`
	// Chain lists the entries in failover order. Entry 0 is the primary.
	Chain []EntrySpec `json:"chain,omitempty"`
}

// RetrySpec is the JSON form of the retry decorator's configuration.
type RetrySpec struct {
	MaxAttempts   int      `json:"max_attempts,omitempty"`
	BaseDelay     Duration `json:"base_delay,omitzero"`
	MaxDelay      Duration `json:"max_delay,omitzero"`
	Multiplier    float64  `json:"multiplier,omitempty"`
	MaxRetryAfter Duration `json:"max_retry_after,omitzero"`
	Disable       bool     `json:"disable,omitempty"`
}

// RoutingSpec selects the router policy for a preset's chain.
type RoutingSpec struct {
	// Policy is "sticky" (the default) or "affinity". Setting FailThreshold
	// or ReprobeAfter without a policy selects affinity.
	Policy string `json:"policy,omitempty"`
	// FailThreshold is the number of consecutive requests on which the
	// session's profile must fail before the session moves to the next
	// entry. A single failure still fails over within its request. Zero
	// means 1. Affinity only.
	FailThreshold int `json:"fail_threshold,omitempty"`
	// ReprobeAfter, when positive, tries the primary entry again after this
	// many requests served elsewhere, so a session returns to the primary
	// once it recovers. A reprobe waits while a warm prompt-cache prefix
	// would make the switch cost more. Zero never reprobes. Affinity only.
	ReprobeAfter int `json:"reprobe_after,omitempty"`
	// FailoverOnContentFilter lets a content-filter refusal move to another
	// provider in the chain.
	FailoverOnContentFilter bool `json:"failover_on_content_filter,omitempty"`
	// FailoverOnAuth lets an authentication or permission failure (a bad or
	// revoked key) move to the next entry. Off by default: such an error is
	// permanent, and moving on hides a broken credential behind another
	// vendor.
	FailoverOnAuth bool `json:"failover_on_auth,omitempty"`
	// Required lists capabilities every request needs.
	Required []types.Capability `json:"required,omitempty"`
}

// Routing policies.
const (
	PolicySticky   = "sticky"
	PolicyAffinity = "affinity"
)

// UsesAffinity reports whether the routing selects the affinity policy:
// named explicitly, or implied by FailThreshold or ReprobeAfter.
func (r RoutingSpec) UsesAffinity() bool {
	return r.Policy == PolicyAffinity || (r.Policy == "" && (r.FailThreshold > 0 || r.ReprobeAfter > 0))
}

// EntrySpec is one chain entry: a provider, a model, and the options that
// make the configuration complete.
type EntrySpec struct {
	// ID is unique within the preset. Empty defaults to "provider/model".
	ID       string       `json:"id,omitempty"`
	Provider string       `json:"provider"`
	Model    string       `json:"model"`
	Options  *OptionsSpec `json:"options,omitempty"`
	// Unset removes inherited option names from the result.
	Unset []string `json:"unset,omitempty"`
	// Inherit is "all" (the default) or "none", which ignores the preset's
	// options.
	Inherit        string     `json:"inherit,omitempty"`
	Retry          *RetrySpec `json:"retry,omitempty"`
	AttemptTimeout Duration   `json:"attempt_timeout,omitzero"`
	BaseURL        string     `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable that holds the key. The key
	// itself is never part of a catalog.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Vertex serves a Google entry through Vertex AI instead of the Gemini
	// API. Empty fields default from GOOGLE_CLOUD_PROJECT and
	// GOOGLE_CLOUD_LOCATION (then "global").
	Vertex *VertexSpec `json:"vertex,omitempty"`
	// Optional drops the whole entry when its credentials are missing.
	Optional bool `json:"optional,omitempty"`
}

// VertexSpec selects Vertex AI for a Google entry. Credentials come from
// Application Default Credentials, never from the catalog.
type VertexSpec struct {
	Project  string `json:"project,omitempty"`
	Location string `json:"location,omitempty"`
}

// Duration is a time.Duration written as a Go duration string ("500ms").
type Duration time.Duration

// MarshalJSON writes the duration string; zero writes "0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"500ms\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	*d = Duration(v)
	return nil
}

// IsZero lets omitempty-like encoding skip an unset duration.
func (d Duration) IsZero() bool { return d == 0 }

// requestOptions converts the options object to types.RequestOptions. The
// prompt cache and server tools are carried separately.
func (o *OptionsSpec) requestOptions() types.RequestOptions {
	if o == nil {
		return types.RequestOptions{}
	}
	out := types.RequestOptions{
		Temperature: o.Temperature, TopP: o.TopP, TopK: o.TopK,
		FrequencyPenalty: o.FrequencyPenalty, PresencePenalty: o.PresencePenalty,
		Seed: o.Seed, MaxOutputTokens: o.MaxOutputTokens, StopSequences: o.Stop,
		ParallelTools: o.ParallelTools,
	}
	if o.Reasoning != nil {
		out.ReasoningEnabled, out.ReasoningEffort, out.ReasoningBudget = o.Reasoning.Enabled, o.Reasoning.Effort, o.Reasoning.Budget
	}
	if o.ToolChoice != "" {
		out.ToolChoice = &types.ToolChoice{Mode: types.ToolChoiceMode(o.ToolChoice)}
	}
	return out.Clone()
}

// optionsSpec converts request options back to the file form.
func optionsSpec(o types.RequestOptions) *OptionsSpec {
	o = o.Clone()
	out := &OptionsSpec{Temperature: o.Temperature, TopP: o.TopP, TopK: o.TopK,
		FrequencyPenalty: o.FrequencyPenalty, PresencePenalty: o.PresencePenalty,
		Seed: o.Seed, MaxOutputTokens: o.MaxOutputTokens, Stop: o.StopSequences, ParallelTools: o.ParallelTools}
	if o.HasReasoning() {
		out.Reasoning = &ReasoningOption{Enabled: o.ReasoningEnabled, Effort: o.ReasoningEffort, Budget: o.ReasoningBudget}
	}
	if o.ToolChoice != nil {
		out.ToolChoice = string(o.ToolChoice.Mode)
	}
	return out
}

// optionNames are the names an entry may unset: the request options plus the
// prompt cache and server tools.
func optionNames() []string {
	return append(types.AllOptionNames(), optionPromptCache, optionServerTools)
}

const (
	optionPromptCache = "prompt_cache"
	optionServerTools = "server_tools"
)

// clone deep-copies an options object.
func (o *OptionsSpec) clone() *OptionsSpec {
	if o == nil {
		return nil
	}
	out := optionsSpec(o.requestOptions())
	if o.Reasoning != nil && out.Reasoning == nil {
		out.Reasoning = &ReasoningOption{}
	}
	out.ToolChoice = o.ToolChoice
	if o.PromptCache != nil {
		pc := *o.PromptCache
		out.PromptCache = &pc
	}
	for _, st := range o.ServerTools {
		out.ServerTools = append(out.ServerTools, serverToolSpec(st.serverTool()))
	}
	return out
}

func serverToolSpec(st types.ServerTool) ServerToolSpec {
	out := ServerToolSpec{Kind: st.Kind, MaxUses: st.MaxUses, UserLocation: st.UserLocation,
		AllowedDomains: append([]string(nil), st.AllowedDomains...), BlockedDomains: append([]string(nil), st.BlockedDomains...)}
	if st.MCPServer != nil {
		out.MCPServer = &MCPServerSpec{Name: st.MCPServer.Name, URL: st.MCPServer.URL,
			AllowedTools: append([]string(nil), st.MCPServer.AllowedTools...), RequireApproval: st.MCPServer.RequireApproval}
	}
	return out
}
