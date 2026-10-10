package catalog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// SchemaVersion is the major version of the catalog file format this package
// writes. A version 1 file still loads: UpgradeV1 converts it, with a
// warning. A file with any other major version is rejected rather than half
// understood.
const SchemaVersion = 2

// SchemaVersionV1 is the earlier file format, read through UpgradeV1.
const SchemaVersionV1 = 1

// Catalog is one catalog document: the models the catalog knows, the
// endpoints that serve them, the offerings that join the two (what may be
// sent to a model on an endpoint), the templates they extend, and named
// presets. It is the JSON file format and the in-memory value hosts pass
// around. Model rows reach adapters through Install; presets never become
// global state.
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
	// ModelTemplates are partial models reachable only through Extends.
	ModelTemplates map[string]ModelSpec `json:"model_templates,omitempty"`
	// Models are the model families Lookup matches, keyed by
	// "<vendor>/<prefix>". The prefix matches model identifiers by longest
	// declared prefix.
	Models map[string]ModelSpec `json:"models,omitempty"`
	// Endpoints are where models are reached.
	Endpoints map[string]EndpointSpec `json:"endpoints,omitempty"`
	// OfferingTemplates are partial offerings reachable only through
	// Extends, an endpoint's default_offering_template or its overrides.
	OfferingTemplates map[string]OfferingSpec `json:"offering_templates,omitempty"`
	// Offerings are the explicit model and endpoint pairs, keyed by
	// (model, endpoint). A model with no offering on an endpoint that
	// serves its vendor gets one from the endpoint's inherit_offerings
	// source or its default_offering_template.
	Offerings []OfferingSpec `json:"offerings,omitempty"`
	// Presets are named, complete provider configurations.
	Presets map[types.PresetName]PresetSpec `json:"presets,omitempty"`
	// DefaultPreset is used by the CLI when neither a preset nor a model is
	// named.
	DefaultPreset types.PresetName `json:"default_preset,omitempty"`
	// Dials are global dials, the lowest layer of every preset entry's.
	Dials *types.Dials `json:"dials,omitempty"`

	// deletedPresets lists the presets a layer removes with null.
	deletedPresets []types.PresetName
	// upgraded is set on a catalog read from a version 1 file.
	upgraded bool
}

// ModelSpec is one model family or model template: the facts about the
// weights. Every field is optional in a template or an overlay; a resolved
// model inherits what it leaves unset from the template it extends.
type ModelSpec struct {
	// Extends names a model template. Chains are allowed up to four deep.
	Extends      string        `json:"extends,omitempty"`
	Tier         Tier          `json:"tier,omitempty"`
	SupersededBy types.ModelID `json:"superseded_by,omitempty"`
	// Limits are the model's token limits. Zero means undeclared.
	Limits *ModelLimitsSpec `json:"limits,omitempty"`
	// Modalities are what the weights can take in and produce. An
	// offering's modalities are narrowed to these.
	Modalities *ModelModalitiesSpec `json:"modalities,omitempty"`
	Notes      []string             `json:"notes,omitempty"`
	// Replace makes an overlay model replace the base model whole instead
	// of patching it. Delete removes the base model and its offerings.
	Replace bool `json:"$replace,omitempty"`
	Delete  bool `json:"$delete,omitempty"`

	raw json.RawMessage
}

// ModelLimitsSpec declares token limits. Nil means undeclared.
type ModelLimitsSpec struct {
	ContextWindow   *int `json:"context_window,omitempty"`
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
}

// ModelModalitiesSpec lists the modalities a model takes in and produces.
// Each list replaces the inherited one.
type ModelModalitiesSpec struct {
	In  []types.Modality `json:"in,omitempty"`
	Out []types.Modality `json:"out,omitempty"`
}

// EndpointSpec is where and how models are reached. Credentials are never
// part of a catalog: auth names a secret reference.
type EndpointSpec struct {
	// Surface is the API the endpoint speaks: anthropic.messages,
	// openai.chat, openai.responses, openai.compatible, gemini.api, vertex
	// or ollama.native.
	Surface string `json:"surface"`
	// Serves lists the vendors whose models the endpoint serves.
	Serves []types.ProviderName `json:"serves,omitempty"`
	// Primary marks the endpoint Lookup resolves a vendor's models on.
	// Each vendor has at most one.
	Primary   bool           `json:"primary,omitempty"`
	Location  *LocationSpec  `json:"location,omitempty"`
	Auth      *AuthSpec      `json:"auth,omitempty"`
	Transport *TransportSpec `json:"transport,omitempty"`
	Capacity  *CapacitySpec  `json:"capacity,omitempty"`
	Data      *DataSpec      `json:"data,omitempty"`
	Files     *FilesSpec     `json:"files,omitempty"`
	Modes     *ModesSpec     `json:"modes,omitempty"`
	// ModelIDs maps a catalog model prefix to the identifier this endpoint
	// takes for it, when the two differ.
	ModelIDs map[types.ModelID]types.ModelID `json:"model_ids,omitempty"`
	// DefaultOfferingTemplate is the offering template a served model with
	// no other offering here gets, and the baseline for a model the catalog
	// does not list (with Known false).
	DefaultOfferingTemplate string `json:"default_offering_template,omitempty"`
	// InheritOfferings names an endpoint whose offerings this one copies
	// for every model it has no explicit offering for.
	InheritOfferings string `json:"inherit_offerings,omitempty"`
	// Overrides patch every offering on this endpoint, after its templates
	// and before an explicit offering's own fields.
	Overrides *OfferingSpec `json:"overrides,omitempty"`

	raw json.RawMessage
}

// LocationSpec places an endpoint. A value of the form "env:NAME" is read
// from the environment when the entry is built.
type LocationSpec struct {
	Region  string `json:"region,omitempty"`
	Project string `json:"project,omitempty"`
}

// AuthSpec says how an endpoint authenticates.
type AuthSpec struct {
	// Type is api_key (the default when a secret is set), adc (Application
	// Default Credentials) or none.
	Type string `json:"type,omitempty"`
	// Secret is a reference to the credential, never the credential:
	// "env:NAME" names an environment variable.
	Secret string `json:"secret,omitempty"`
}

// TransportSpec is how requests reach the endpoint.
type TransportSpec struct {
	// BaseURL targets a gateway or a compatible server.
	BaseURL string `json:"base_url,omitempty"`
	// Timeout bounds one attempt.
	Timeout Duration `json:"timeout,omitzero"`
}

// CapacitySpec is the endpoint's declared rate limits. Zero means
// undeclared.
type CapacitySpec struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	TokensPerMinute   int `json:"tokens_per_minute,omitempty"`
	MaxConcurrency    int `json:"max_concurrency,omitempty"`
}

// DataSpec is what the endpoint does with request data.
type DataSpec struct {
	ZeroRetention bool   `json:"zero_retention,omitempty"`
	Store         *bool  `json:"store,omitempty"`
	Residency     string `json:"residency,omitempty"`
	// PIIOK means raw personal data may be sent here.
	PIIOK bool `json:"pii_ok,omitempty"`
}

// FilesSpec is the endpoint's vendor file store and the URI schemes it
// fetches.
type FilesSpec struct {
	API        bool     `json:"api,omitempty"`
	MaxBytes   int64    `json:"max_bytes,omitempty"`
	TTL        Duration `json:"ttl,omitzero"`
	URISchemes []string `json:"uri_schemes,omitempty"`
}

// ModesSpec lists the transport modes the endpoint offers.
type ModesSpec struct {
	// Batch means the endpoint has a batch API, which a batch service
	// tier needs.
	Batch bool `json:"batch,omitempty"`
	// Streaming false declares an endpoint that cannot stream. Nil means
	// it can.
	Streaming *bool `json:"streaming,omitempty"`
}

// OfferingSpec is one offering, offering template or endpoint override:
// what may be sent to a model on an endpoint. Every field is optional in a
// template or an overlay.
type OfferingSpec struct {
	// Model and Endpoint identify an explicit offering. Model is the
	// "<vendor>/<prefix>" key of a model.
	Model    string `json:"model,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// Extends names an offering template. Chains are allowed up to four
	// deep.
	Extends string `json:"extends,omitempty"`
	// Features are the capabilities that are not request parameters (tools,
	// streaming, web_search, ...). A non-empty list replaces the inherited
	// one; AddFeatures and RemoveFeatures then edit it.
	Features       []types.Capability `json:"features,omitempty"`
	AddFeatures    []types.Capability `json:"add_features,omitempty"`
	RemoveFeatures []types.Capability `json:"remove_features,omitempty"`
	// Params declares the request parameters, merged per parameter and
	// field. A parameter is accepted when allowed resolves true, or when it
	// declares a type and allowed is unset. Null removes an inherited one.
	Params map[types.ParamName]*ParamSpec `json:"params,omitempty"`
	// Constraints restrict parameters together, keyed by a name so a child
	// can replace or (with null) remove an inherited one.
	Constraints map[string]*types.Constraint `json:"constraints,omitempty"`
	Modalities  *ModalitiesSpec              `json:"modalities,omitempty"`
	// Limits, when set, cap the model's limits on this endpoint. A baseline
	// template declares the limits of an unlisted model here.
	Limits *ModelLimitsSpec `json:"limits,omitempty"`
	// StructuredOutput is "", "native" or "tool_call". Nil inherits.
	StructuredOutput *types.StructuredOutputMode `json:"structured_output,omitempty"`
	ServerTools      []types.ServerToolKind      `json:"server_tools,omitempty"`
	// ServerToolFees replace the inherited fees whole.
	ServerToolFees map[types.ServerToolKind]Fee `json:"server_tool_fees,omitempty"`
	// Pricing is the standard tier's rate card. It replaces the inherited
	// card whole.
	Pricing *PricingSpec `json:"pricing,omitempty"`
	// Tiers are the other service tiers (priority, flex, batch), replaced
	// per tier; null removes one.
	Tiers map[types.ServiceTier]*TierSpec `json:"tiers,omitempty"`
	// ModalityPricing prices modalities the vendor bills apart from text,
	// replaced per modality.
	ModalityPricing map[types.Modality]*types.ModalityRate `json:"modality_pricing,omitempty"`
	// Defaults are option defaults, the lowest declared layer of a preset
	// entry's options.
	Defaults *OptionsSpec `json:"defaults,omitempty"`
	// Dials declares how the offering compiles model-neutral dials, and its
	// default dials. Templates and offerings merge it field by field.
	Dials *DialsSpec `json:"dials,omitempty"`
	// Fallback names offerings to consider when this one cannot serve. It
	// never creates failover by itself.
	Fallback *FallbackSpec `json:"fallback,omitempty"`
	Notes    []string      `json:"notes,omitempty"`
	// Replace makes an overlay offering replace the base offering whole.
	// Delete removes it.
	Replace bool `json:"$replace,omitempty"`
	Delete  bool `json:"$delete,omitempty"`

	raw json.RawMessage
}

// ParamSpec is the file form of types.ParamSpec. Pointer fields distinguish
// "unset, inherit" from an explicit value.
type ParamSpec struct {
	// Type is number, integer, boolean, enum or string_list.
	Type   string   `json:"type,omitempty"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Values []string `json:"values,omitempty"`
	// Default is the value the vendor applies when the parameter is not
	// sent.
	Default  any   `json:"default,omitempty"`
	Allowed  *bool `json:"allowed,omitempty"`
	Required *bool `json:"required,omitempty"`
	// Special maps values outside the range to their meaning, such as
	// {"-1": "dynamic", "0": "off"}. An empty meaning removes one.
	Special map[string]string `json:"special,omitempty"`
	// Wire is the vendor's field name, for documentation.
	Wire string `json:"wire,omitempty"`
}

// ModalitiesSpec is the file form of types.Modalities. Each modality merges
// field by field; null removes an inherited one.
type ModalitiesSpec struct {
	In  map[types.Modality]*ModalityLimitSpec `json:"in,omitempty"`
	Out map[types.Modality]*ModalityLimitSpec `json:"out,omitempty"`
	// ToolResult is inline, follow_up_user or none per modality. An empty
	// value removes one.
	ToolResult map[types.Modality]string `json:"tool_result,omitempty"`
}

// ModalityLimitSpec is the file form of types.ModalityLimit.
type ModalityLimitSpec struct {
	// Media lists the accepted media types. It replaces the inherited list.
	Media []types.MediaType `json:"media,omitempty"`
	// Sources lists the accepted locators: inline, uri, file.
	Sources     []types.SourceKind `json:"sources,omitempty"`
	MaxBytes    *int64             `json:"max_bytes,omitempty"`
	MaxCount    *int               `json:"max_count,omitempty"`
	MaxPixels   *int               `json:"max_pixels,omitempty"`
	MaxPages    *int               `json:"max_pages,omitempty"`
	MaxDuration *Duration          `json:"max_duration,omitempty"`
	FPS         *ParamSpec         `json:"fps,omitempty"`
	Tokens      *types.TokenRule   `json:"tokens,omitempty"`
}

// PricingSpec is the JSON form of a standard-tier rate card. Rates are per
// million tokens. Batch rates live on the batch tier.
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
	p.BatchDiscount, p.BatchCachedInputPerMTok, p.Modal = 0, 0, nil // modal rates are the offering's ModalityPricing
	if reflect.DeepEqual(p, types.Pricing{}) {
		return nil
	}
	return &PricingSpec{Currency: p.Currency, InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok,
		CachedInputPerMTok: p.CachedInputPerMTok, CacheWritePerMTok: p.CacheWritePerMTok,
		PerRequest: p.PerRequest, Free: p.Free, AsOf: p.AsOf, Source: p.Source}
}

// TierSpec is the file form of types.TierSpec.
type TierSpec struct {
	// Transport is "batch" for a tier served through the endpoint's batch
	// mode.
	Transport string `json:"transport,omitempty"`
	// Discount is the fraction taken off the standard token rates.
	Discount float64 `json:"discount,omitempty"`
	// CachedInputPerMTok is the cache-read rate on this tier, for vendors
	// whose discount does not stack with the cache discount.
	CachedInputPerMTok float64 `json:"cached_input_per_mtok,omitempty"`
	// Pricing, when set, is the tier's rate card outright.
	Pricing *PricingSpec `json:"pricing,omitempty"`
	// Wire is the vendor's name for the tier.
	Wire string `json:"wire,omitempty"`
}

// FallbackSpec names offerings ("<vendor>/<prefix>@<endpoint>") to consider
// when an offering cannot serve.
type FallbackSpec struct {
	Equivalents   []string `json:"equivalents,omitempty"`
	LargerContext []string `json:"larger_context,omitempty"`
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
	Extends types.PresetName `json:"extends,omitempty"`
	// Options are inherited by every chain entry that does not opt out.
	Options *OptionsSpec `json:"options,omitempty"`
	// Dials are inherited by every chain entry that does not opt out, and
	// compiled for each entry's own model.
	Dials *types.Dials `json:"dials,omitempty"`
	// ToolChoice is the agent default: auto, none, required or named:<tool>.
	ToolChoice string `json:"tool_choice,omitempty"`
	// OutputMode is auto, native, tool or prompt.
	OutputMode string `json:"output_mode,omitempty"`
	// LLMTimeout bounds one provider call, as a Go duration string.
	LLMTimeout Duration `json:"llm_timeout,omitzero"`
	// Compaction is the agent's default compaction strategy.
	Compaction *CompactionSpec `json:"compaction,omitempty"`
	// Retry is the default retry policy of every entry.
	Retry   *RetrySpec   `json:"retry,omitempty"`
	Routing *RoutingSpec `json:"routing,omitempty"`
	// RequireDeclared requires every entry's model to be an exact row.
	RequireDeclared *bool `json:"require_declared,omitempty"`
	// Chain lists the entries in failover order. Entry 0 is the primary.
	Chain []EntrySpec `json:"chain,omitempty"`
}

// CompactionSpec is the JSON form of types.CompactConfig.
type CompactionSpec struct {
	// Strategy is none, sliding_window, summarize, clear_tool_results,
	// keep_recent, summary, relevant_plus_summary or chain.
	Strategy        string           `json:"strategy"`
	MaxInputTokens  int              `json:"max_input_tokens,omitempty"`
	TargetTokens    int              `json:"target_tokens,omitempty"`
	KeepTurns       int              `json:"keep_turns,omitempty"`
	SelectK         int              `json:"select_k,omitempty"`
	Threshold       int              `json:"threshold,omitempty"`
	KeepLast        int              `json:"keep_last,omitempty"`
	WindowSize      int              `json:"window_size,omitempty"`
	KeepToolResults int              `json:"keep_tool_results,omitempty"`
	ExcludeTools    []string         `json:"exclude_tools,omitempty"`
	SummaryModel    string           `json:"summary_model,omitempty"`
	Chain           []CompactionSpec `json:"chain,omitempty"`
}

// Config converts the spec to a types.CompactConfig.
func (s CompactionSpec) Config() types.CompactConfig {
	cc := types.CompactConfig{
		Strategy: types.CompactStrategy(s.Strategy), MaxInputTokens: s.MaxInputTokens, TargetTokens: s.TargetTokens,
		KeepTurns: s.KeepTurns, SelectK: s.SelectK, Threshold: s.Threshold, KeepLast: s.KeepLast,
		WindowSize: s.WindowSize, KeepToolResults: s.KeepToolResults, SummaryModel: s.SummaryModel,
		ExcludeTools: append([]string(nil), s.ExcludeTools...),
	}
	for _, step := range s.Chain {
		cc.Chain = append(cc.Chain, step.Config())
	}
	return cc
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
	ID string `json:"id,omitempty"`
	// Offering names the entry's offering as "<vendor>/<model>@<endpoint>".
	// It sets the provider, model and endpoint together.
	Offering string `json:"offering,omitempty"`
	// Endpoint names the endpoint that serves Model. Empty uses the
	// vendor's primary endpoint, or the one vertex, base_url or
	// api_key_env describe.
	Endpoint string `json:"endpoint,omitempty"`
	// Provider is the vendor. It may be left out with an offering or an
	// endpoint, which name it.
	Provider types.ProviderName `json:"provider,omitempty"`
	Model    types.ModelID      `json:"model,omitempty"`
	Options  *OptionsSpec       `json:"options,omitempty"`
	// Dials are the entry's own dials, on top of the preset's.
	Dials *types.Dials `json:"dials,omitempty"`
	// Unset removes inherited option names from the result, and inherited
	// dials named "dials.<name>".
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
	// LocalFallback applies to an Ollama entry: when Model is not pulled on
	// the server, preset.Build serves another pulled chat model instead and
	// records a warning. With no chat model pulled, the entry fails (or is
	// dropped when Optional) with preset.ErrNoLocalModel.
	LocalFallback bool `json:"local_fallback,omitempty"`
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
