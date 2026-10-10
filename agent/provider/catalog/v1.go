package catalog

import (
	"encoding/json"

	"github.com/urmzd/saige/agent/types"
)

// CatalogV1 is the version 1 file format: model rows that mix model facts
// with request knobs, templates they extend, per-provider baselines and
// presets. It is read only to be converted with UpgradeV1.
type CatalogV1 struct {
	Schema         string                          `json:"$schema,omitempty"`
	Version        int                             `json:"version"`
	Revision       string                          `json:"revision,omitempty"`
	InheritDefault *bool                           `json:"inherit_default,omitempty"`
	Templates      map[string]ModelSpecV1          `json:"templates,omitempty"`
	Models         []ModelSpecV1                   `json:"models,omitempty"`
	Baselines      map[string]ModelSpecV1          `json:"baselines,omitempty"`
	Presets        map[types.PresetName]PresetSpec `json:"presets,omitempty"`
	DefaultPreset  types.PresetName                `json:"default_preset,omitempty"`
	Dials          *types.Dials                    `json:"dials,omitempty"`

	deletedPresets []types.PresetName
}

// ModelSpecV1 is one version 1 model row, template or baseline.
type ModelSpecV1 struct {
	Provider             types.ProviderName           `json:"provider,omitempty"`
	Prefix               types.ModelID                `json:"prefix,omitempty"`
	Extends              string                       `json:"extends,omitempty"`
	Tier                 Tier                         `json:"tier,omitempty"`
	SupersededBy         types.ModelID                `json:"superseded_by,omitempty"`
	ChatCompletionsTools string                       `json:"chat_completions_tools,omitempty"`
	Capabilities         []types.Capability           `json:"capabilities,omitempty"`
	AddCapabilities      []types.Capability           `json:"add_capabilities,omitempty"`
	RemoveCapabilities   []types.Capability           `json:"remove_capabilities,omitempty"`
	Limits               *LimitsSpecV1                `json:"limits,omitempty"`
	Reasoning            *ReasoningSpecV1             `json:"reasoning,omitempty"`
	StructuredOutput     *types.StructuredOutputMode  `json:"structured_output,omitempty"`
	Media                []types.MediaType            `json:"media,omitempty"`
	ServerTools          []types.ServerToolKind       `json:"server_tools,omitempty"`
	ServerToolFees       map[types.ServerToolKind]Fee `json:"server_tool_fees,omitempty"`
	Pricing              *PricingSpecV1               `json:"pricing,omitempty"`
	Defaults             *OptionsSpec                 `json:"defaults,omitempty"`
	Dials                *DialsSpec                   `json:"dials,omitempty"`
	Notes                []string                     `json:"notes,omitempty"`
	Replace              bool                         `json:"$replace,omitempty"`
	Delete               bool                         `json:"$delete,omitempty"`

	raw json.RawMessage
}

// LimitsSpecV1 declares version 1 token limits.
type LimitsSpecV1 struct {
	ContextWindow          *int `json:"context_window,omitempty"`
	MaxOutputTokens        *int `json:"max_output_tokens,omitempty"`
	DefaultMaxOutputTokens *int `json:"default_max_output_tokens,omitempty"`
}

// ReasoningSpecV1 declares how a version 1 family sizes its reasoning.
type ReasoningSpecV1 struct {
	Efforts                     []string           `json:"efforts,omitempty"`
	DefaultEffort               *string            `json:"default_effort,omitempty"`
	Required                    *bool              `json:"required,omitempty"`
	DefaultEnabled              *bool              `json:"default_enabled,omitempty"`
	MinBudget                   *int               `json:"min_budget,omitempty"`
	MaxBudget                   *int               `json:"max_budget,omitempty"`
	DynamicBudget               *bool              `json:"dynamic_budget,omitempty"`
	ZeroBudget                  *bool              `json:"zero_budget,omitempty"`
	SamplingRequiresNoReasoning []types.Capability `json:"sampling_requires_no_reasoning,omitempty"`
	ForcedToolChoice            *bool              `json:"forced_tool_choice,omitempty"`
}

// PricingSpecV1 is the version 1 rate card, with batch rates inline.
type PricingSpecV1 struct {
	Currency                string  `json:"currency,omitempty"`
	InputPerMTok            float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok           float64 `json:"output_per_mtok,omitempty"`
	CachedInputPerMTok      float64 `json:"cached_input_per_mtok,omitempty"`
	CacheWritePerMTok       float64 `json:"cache_write_per_mtok,omitempty"`
	PerRequest              float64 `json:"per_request,omitempty"`
	BatchDiscount           float64 `json:"batch_discount,omitempty"`
	BatchCachedInputPerMTok float64 `json:"batch_cached_input_per_mtok,omitempty"`
	Free                    bool    `json:"free,omitempty"`
	AsOf                    string  `json:"as_of,omitempty"`
	Source                  string  `json:"source,omitempty"`
}
