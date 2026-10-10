package types

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// An offering is a model served through one endpoint: what may actually be
// sent there. The model holds the facts about the weights (limits, the
// modalities it can take and produce); the endpoint says where and how it is
// reached; the offering joins them with the parameter specification,
// modality limits, service tiers and pricing that hold on that endpoint.
// The catalog owns the file form; this is the read model.

// Endpoint surfaces: the API an endpoint speaks.
const (
	SurfaceAnthropicMessages = "anthropic.messages"
	SurfaceOpenAIChat        = "openai.chat"
	SurfaceOpenAIResponses   = "openai.responses"
	SurfaceOpenAICompatible  = "openai.compatible"
	SurfaceGeminiAPI         = "gemini.api"
	SurfaceVertex            = "vertex"
	SurfaceOllamaNative      = "ollama.native"
)

// ParamPromptCacheRetention is the prompt cache retention an offering
// accepts ("in_memory", "24h"). It is checked when a prompt cache is
// configured, since the retention is not a request option.
const ParamPromptCacheRetention ParamName = "prompt_cache.retention"

// Meanings of the special reasoning budget values.
const (
	SpecialDynamic = "dynamic"
	SpecialOff     = "off"
)

// ServiceTier is a vendor service class an offering is sold under.
type ServiceTier string

const (
	ServiceStandard ServiceTier = "standard"
	ServicePriority ServiceTier = "priority"
	ServiceFlex     ServiceTier = "flex"
	// ServiceBatch is the vendor's batch API. Its transport needs the
	// endpoint's batch mode.
	ServiceBatch ServiceTier = "batch"
)

// TransportBatch marks a tier served through the endpoint's batch mode.
const TransportBatch = "batch"

// TierSpec is how one service tier is reached and priced.
type TierSpec struct {
	// Transport is TransportBatch for a tier served by the endpoint's batch
	// mode, empty for the interactive API.
	Transport string `json:"transport,omitempty"`
	// Discount is the fraction taken off the standard token rates, such as
	// 0.5 for half price.
	Discount float64 `json:"discount,omitempty"`
	// CachedInputPerMTok is the cache-read rate on this tier, for vendors
	// whose discount does not simply stack with the cache discount. Zero
	// applies Discount to the standard cache-read rate.
	CachedInputPerMTok float64 `json:"cached_input_per_mtok,omitempty"`
	// Pricing, when set, is this tier's rate card outright.
	Pricing *Pricing `json:"pricing,omitempty"`
	// Wire is the vendor's name for the tier, such as OpenAI's
	// service_tier value.
	Wire string `json:"wire,omitempty"`
}

// ModalityRate prices one modality where the vendor bills it apart from
// text tokens. Rates are per million tokens of that modality.
type ModalityRate struct {
	InputPerMTok  float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok float64 `json:"output_per_mtok,omitempty"`
}

// Fee is a flat per-use charge for a provider-executed tool, such as a web
// search billed per query on top of the tokens it adds to the prompt.
type Fee struct {
	Currency string  `json:"currency,omitempty"` // ISO code; empty means DefaultCurrency
	PerUse   float64 `json:"per_use"`            // charge for one invocation
	AsOf     string  `json:"as_of,omitempty"`    // date the rate was recorded
	Source   string  `json:"source,omitempty"`   // where the rate came from
}

// Reserve returns the most a call can spend on the tool when the provider may
// invoke it up to maxUses times. A budget reserves this before dispatch,
// because the invocations happen inside the provider and are only reported
// after the fact. maxUses <= 0 means the provider default, which is
// unbounded, so Reserve reports false.
func (f Fee) Reserve(maxUses int) (float64, bool) {
	if maxUses <= 0 {
		return 0, false
	}
	return f.PerUse * float64(maxUses), true
}

// ModelInfo is what the catalog knows about the weights.
type ModelInfo struct {
	Vendor ProviderName `json:"vendor"`
	// Prefix is the family the catalog matched, empty for a baseline.
	Prefix          ModelID    `json:"prefix,omitempty"`
	Tier            string     `json:"tier,omitempty"`
	SupersededBy    ModelID    `json:"superseded_by,omitempty"`
	ContextWindow   int        `json:"context_window,omitempty"`
	MaxOutputTokens int        `json:"max_output_tokens,omitempty"`
	In              []Modality `json:"in,omitempty"`
	Out             []Modality `json:"out,omitempty"`
	// Known means the model is declared. A baseline offering for an
	// unlisted model has Known false.
	Known bool     `json:"known"`
	Notes []string `json:"notes,omitempty"`
}

// DataHandling is what an endpoint does with request data.
type DataHandling struct {
	ZeroRetention bool   `json:"zero_retention,omitempty"`
	Store         *bool  `json:"store,omitempty"`
	Residency     string `json:"residency,omitempty"`
	// PIIOK means raw personal data may be sent to this endpoint.
	PIIOK bool `json:"pii_ok,omitempty"`
}

// FileSupport is an endpoint's vendor file store.
type FileSupport struct {
	API        bool          `json:"api,omitempty"`
	MaxBytes   int64         `json:"max_bytes,omitempty"`
	TTL        time.Duration `json:"ttl,omitempty"`
	URISchemes []string      `json:"uri_schemes,omitempty"`
}

// EndpointInfo is the part of an endpoint an offering carries.
type EndpointInfo struct {
	Name    string `json:"name"`
	Surface string `json:"surface,omitempty"`
	// ModelID is the identifier the endpoint takes for the model, when it
	// differs from the catalog's.
	ModelID   ModelID      `json:"model_id,omitempty"`
	Data      DataHandling `json:"data,omitzero"`
	Files     FileSupport  `json:"files,omitzero"`
	Batch     bool         `json:"batch,omitempty"`
	Streaming bool         `json:"streaming,omitempty"`
}

// TokenRule estimates the tokens one piece of media costs.
type TokenRule struct {
	Base      int `json:"base,omitempty"`
	PerTile   int `json:"per_tile,omitempty"`
	Tile      int `json:"tile,omitempty"`
	PerPage   int `json:"per_page,omitempty"`
	PerSecond int `json:"per_second,omitempty"`
	PerImage  int `json:"per_image,omitempty"`
	PerPixels int `json:"per_pixels,omitempty"`
}

// ModalityLimit is what an offering accepts of one modality. A zero limit
// is undeclared, not unlimited.
type ModalityLimit struct {
	Media       []MediaType   `json:"media,omitempty"`
	Sources     []SourceKind  `json:"sources,omitempty"`
	MaxBytes    int64         `json:"max_bytes,omitempty"`
	MaxCount    int           `json:"max_count,omitempty"`
	MaxPixels   int           `json:"max_pixels,omitempty"`
	MaxPages    int           `json:"max_pages,omitempty"`
	MaxDuration time.Duration `json:"max_duration,omitempty"`
	FPS         *ParamSpec    `json:"fps,omitempty"`
	Tokens      TokenRule     `json:"tokens,omitzero"`
}

// Tool-result lowering: how a modality inside a tool result reaches the
// model.
const (
	ToolResultInline       = "inline"
	ToolResultFollowUpUser = "follow_up_user"
	ToolResultNone         = "none"
)

// Modalities are the modalities an offering takes in and produces, with
// their limits.
type Modalities struct {
	In  map[Modality]ModalityLimit `json:"in,omitempty"`
	Out map[Modality]ModalityLimit `json:"out,omitempty"`
	// ToolResult says how each modality inside a tool result is sent:
	// ToolResultInline, ToolResultFollowUpUser or ToolResultNone.
	ToolResult map[Modality]string `json:"tool_result,omitempty"`
}

// MediaTypes returns the union of the input media types, sorted.
func (m Modalities) MediaTypes() []MediaType {
	var out []MediaType
	for _, l := range m.In {
		for _, mt := range l.Media {
			if !slices.Contains(out, mt) {
				out = append(out, mt)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Accepts reports whether a media type is accepted as input, and its limit.
func (m Modalities) Accepts(mt MediaType) (ModalityLimit, bool) {
	l, ok := m.In[mt.Modality()]
	if !ok || !slices.Contains(l.Media, mt) {
		return ModalityLimit{}, false
	}
	return l, true
}

// FallbackHints name offerings to consider when this one cannot serve.
// They never create failover by themselves.
type FallbackHints struct {
	Equivalents   []string `json:"equivalents,omitempty"`
	LargerContext []string `json:"larger_context,omitempty"`
}

// Offering is one model served through one endpoint.
type Offering struct {
	// ID is "<vendor>/<model prefix>@<endpoint>".
	ID       string       `json:"id"`
	Model    ModelInfo    `json:"model"`
	Endpoint EndpointInfo `json:"endpoint"`
	// Features are the capabilities that are not request parameters, such
	// as tools, streaming or web_search, sorted.
	Features         []Capability              `json:"features,omitempty"`
	Params           ParamSpace                `json:"params,omitzero"`
	Modalities       Modalities                `json:"modalities,omitzero"`
	StructuredOutput StructuredOutputMode      `json:"structured_output,omitempty"`
	ServerTools      []ServerToolKind          `json:"server_tools,omitempty"`
	ServerToolFees   map[ServerToolKind]Fee    `json:"server_tool_fees,omitempty"`
	Pricing          Pricing                   `json:"pricing,omitzero"` // the standard tier
	Tiers            map[ServiceTier]TierSpec  `json:"tiers,omitempty"`
	ModalityPricing  map[Modality]ModalityRate `json:"modality_pricing,omitempty"`
	Defaults         RequestOptions            `json:"-"`
	DialMap          DialMap                   `json:"-"`
	Fallback         FallbackHints             `json:"fallback,omitzero"`
	Notes            []string                  `json:"notes,omitempty"`
}

// knob pairs a request parameter with the capability that declares it.
type knob struct {
	param ParamName
	cap   Capability
}

// knobs lists every parameter a capability flag declares, in validation
// order.
var knobs = []knob{
	{ParamTemperature, CapTemperature}, {ParamTopP, CapTopP}, {ParamTopK, CapTopK},
	{ParamFrequencyPenalty, CapFrequencyPenalty}, {ParamPresencePenalty, CapPresencePenalty},
	{ParamSeed, CapSeed}, {ParamMaxOutputTokens, CapMaxOutputTokens}, {ParamStop, CapStopSequences},
	{ParamParallelTools, CapParallelToolControl}, {ParamToolChoice, CapToolChoice},
	{ParamReasoningEnabled, CapReasoningToggle}, {ParamReasoningEffort, CapReasoningEffort},
	{ParamReasoningBudget, CapReasoningBudget},
}

// ParamCapability returns the capability that declares a parameter, and
// false for a parameter no capability flag stands for.
func ParamCapability(p ParamName) (Capability, bool) {
	for _, k := range knobs {
		if k.param == p {
			return k.cap, true
		}
	}
	return "", false
}

// CapabilityParam returns the parameter a capability flag declares, and
// false for a feature capability.
func CapabilityParam(c Capability) (ParamName, bool) {
	for _, k := range knobs {
		if k.cap == c {
			return k.param, true
		}
	}
	return "", false
}

// IsFeature reports whether a capability is a feature rather than a
// request parameter.
func IsFeature(c Capability) bool {
	_, ok := CapabilityParam(c)
	return !ok
}

// reasoningParams are the three reasoning controls.
var reasoningParams = []ParamName{ParamReasoningEnabled, ParamReasoningBudget, ParamReasoningEffort}

// Constraint names the catalog writes for the rules a capability
// declaration implies.
const (
	ConstraintReasoningExclusive = "reasoning.exclusive"
	ConstraintChatTools          = "openai.chat.tools"
)

// ReasoningExclusive is the rule that a request sets at most one reasoning
// control.
func ReasoningExclusive() Constraint {
	return Constraint{Exclusive: slices.Clone(reasoningParams)}
}

// SamplingConstraints forbid the given parameters whenever reasoning is
// active, as ModelCapabilities.ReasoningActive decides it: one constraint
// per reasoning control, keyed by the catalog's constraint names
// ("sampling.<control>").
func SamplingConstraints(forbid []ParamName) map[string]Constraint {
	t, f := true, false
	unset := CondValue{Set: &f}
	return map[string]Constraint{
		// The control the request sets decides; with none set, the
		// defaults do.
		"sampling." + string(ParamReasoningEnabled): {When: Condition{string(ParamReasoningEnabled): {Bool: &t},
			string(ParamReasoningBudget): unset, string(ParamReasoningEffort): unset}, Forbid: slices.Clone(forbid)},
		"sampling." + string(ParamReasoningBudget): {When: Condition{string(ParamReasoningBudget): {Set: &t, Not: []string{"0"}}},
			Forbid: slices.Clone(forbid)},
		"sampling." + string(ParamReasoningEffort): {When: Condition{string(ParamReasoningEffort): {Not: []string{reasoningEffortNone}},
			string(ParamReasoningEnabled): unset, string(ParamReasoningBudget): unset}, Forbid: slices.Clone(forbid)},
	}
}

// ChatToolsConstraint is the OpenAI Chat Completions rule for tools on a
// reasoning model, or false for ChatToolsAny.
func ChatToolsConstraint(rule ChatCompletionsTools) (Constraint, bool) {
	t := true
	when := Condition{CondRequestTools: {Bool: &t}, CondRequestSurface: {In: []string{SurfaceOpenAIChat}}}
	switch rule {
	case ChatToolsNoReasoning:
		return Constraint{When: when, Require: map[ParamName][]string{ParamReasoningEffort: {reasoningEffortNone}},
			Reason: "Chat Completions takes tools only with reasoning effort none"}, true
	case ChatToolsResponsesOnly:
		return Constraint{When: when, Forbid: []ParamName{CondRequestTools},
			Reason: "tools need the Responses API"}, true
	}
	return Constraint{}, false
}

// isReasoningCondition reports whether a condition tests only reasoning
// controls.
func isReasoningCondition(c Condition) bool {
	if len(c) == 0 {
		return false
	}
	for k := range c {
		if !slices.Contains(reasoningParams, ParamName(k)) {
			return false
		}
	}
	return true
}

// chatToolsRule recognizes a ChatToolsConstraint.
func chatToolsRule(c Constraint) (ChatCompletionsTools, bool) {
	tools, surface := c.When[CondRequestTools], c.When[CondRequestSurface]
	if len(c.When) != 2 || tools.Bool == nil || !*tools.Bool || !slices.Equal(surface.In, []string{SurfaceOpenAIChat}) {
		return "", false
	}
	switch {
	case slices.Equal(c.Forbid, []ParamName{CondRequestTools}):
		return ChatToolsResponsesOnly, true
	case slices.Equal(c.Require[ParamReasoningEffort], []string{reasoningEffortNone}) && len(c.Require) == 1:
		return ChatToolsNoReasoning, true
	}
	return "", false
}

// Accepted reports whether the offering accepts a parameter.
func (o Offering) Accepted(p ParamName) bool {
	s, ok := o.Params.Params[p]
	return ok && s.Accepted()
}

// AcceptsValue checks one value of an enum parameter, such as
// ParamPromptCacheRetention. An undeclared parameter accepts any value, so
// a model the catalog says nothing about is not refused. Errors match
// ErrInvalidModelConfig and name the parameter.
func (o Offering) AcceptsValue(p ParamName, value string) error {
	s, ok := o.Params.Params[p]
	if !ok {
		return nil
	}
	if !s.Accepted() {
		return o.optionError(string(p), "not declared supported for this model")
	}
	if len(s.Values) > 0 && !slices.Contains(s.Values, value) {
		return o.optionError(string(p), fmt.Sprintf("%q is not accepted; must be one of %v", value, s.Values))
	}
	return nil
}

func (o Offering) optionError(option, reason string) error {
	return &ProviderError{Provider: string(o.Model.Vendor), Model: string(o.Model.Prefix), Kind: ErrorKindPermanent,
		Err: fmt.Errorf("%w: %s: %s on %s", ErrInvalidModelConfig, option, reason, o.Endpoint.Name)}
}

// TierPricing returns the rate card of a service tier: the standard
// pricing, or a declared tier's own card or discounted rates. The bool is
// false for a tier the offering does not declare.
func (o Offering) TierPricing(t ServiceTier) (Pricing, bool) {
	p := o.Pricing
	p.BatchDiscount, p.BatchCachedInputPerMTok = 0, 0
	p.Modal = o.modal()
	if t == "" || t == ServiceStandard {
		return p, true
	}
	spec, ok := o.Tiers[t]
	if !ok {
		return Pricing{}, false
	}
	if spec.Pricing != nil {
		return *spec.Pricing, true
	}
	if p.IsZero() || p.Free {
		return p, true
	}
	keep := 1 - spec.Discount
	cached := p.CachedInputPerMTok * keep
	if spec.CachedInputPerMTok > 0 {
		cached = spec.CachedInputPerMTok
	}
	p.InputPerMTok *= keep
	p.OutputPerMTok *= keep
	p.CacheWritePerMTok *= keep
	p.CachedInputPerMTok = cached
	p.Modal = scaleModal(p.Modal, keep)
	return p, true
}

// modal returns the offering's modality rates for its standard rate card:
// a copy of ModalityPricing, or nil when the card is unpriced or free.
func (o Offering) modal() map[Modality]ModalityRate {
	if o.Pricing.IsZero() || o.Pricing.Free || len(o.ModalityPricing) == 0 {
		return nil
	}
	return maps.Clone(o.ModalityPricing)
}

// Capabilities projects the offering onto ModelCapabilities, so the
// router, budgets, request validation and decorators keep reading the
// model-level surface they always have. The projection loses nothing a
// capability declaration can state: OfferingFromCapabilities is its
// inverse. The result carries a copy of the offering in Offering.
func (o Offering) Capabilities() ModelCapabilities {
	mc := ModelCapabilities{
		Provider: string(o.Model.Vendor), Model: string(o.Model.Prefix), Family: string(o.Model.Prefix),
		Caps:          map[Capability]bool{},
		ContextWindow: o.Model.ContextWindow, MaxOutputTokens: o.Model.MaxOutputTokens,
		StructuredOutput: o.StructuredOutput,
		Known:            o.Model.Known,
		Media:            ContentSupport{NativeTypes: map[MediaType]bool{}},
	}
	for _, c := range o.Features {
		mc.Caps[c] = true
	}
	for _, k := range knobs {
		if o.Accepted(k.param) {
			mc.Caps[k.cap] = true
		}
	}
	ps := o.Params.Params
	if s, ok := ps[ParamMaxOutputTokens]; ok {
		mc.DefaultMaxOutputTokens = intOf(s.Default)
	}
	if s, ok := ps[ParamReasoningEffort]; ok {
		mc.ReasoningEfforts = slices.Clone(s.Values)
		mc.DefaultReasoningEffort, _ = s.Default.(string)
	}
	if s, ok := ps[ParamReasoningBudget]; ok {
		if s.Min != nil {
			mc.MinReasoningBudget = int(*s.Min)
		}
		if s.Max != nil {
			mc.MaxReasoningBudget = int(*s.Max)
		}
		_, mc.DynamicReasoningBudget = s.Special["-1"]
		_, mc.ZeroReasoningBudget = s.Special["0"]
	}
	if s, ok := ps[ParamReasoningEnabled]; ok {
		mc.ReasoningDefaultEnabled, _ = s.Default.(bool)
	}
	for _, p := range reasoningParams {
		mc.ReasoningRequired = mc.ReasoningRequired || ps[p].Required
	}
	if s, ok := ps[ParamToolChoice]; ok && s.Values != nil {
		mc.RejectsForcedToolChoice = !slices.Contains(s.Values, string(ToolChoiceRequired))
	}
	for _, c := range o.Params.Constraints {
		if rule, ok := chatToolsRule(c); ok {
			mc.ChatCompletionsTools = rule
			continue
		}
		if isReasoningCondition(c.When) && len(c.Forbid) > 0 && len(c.Require) == 0 && len(c.Exclusive) == 0 {
			for _, p := range c.Forbid {
				cp := Capability(p)
				if k, ok := ParamCapability(p); ok {
					cp = k
				}
				if !slices.Contains(mc.SamplingRequiresNoReasoning, cp) {
					mc.SamplingRequiresNoReasoning = append(mc.SamplingRequiresNoReasoning, cp)
				}
			}
		}
	}
	mc.DialMap = o.DialMap.Clone()
	mc.Pricing = o.Pricing
	mc.Pricing.Modal = o.modal()
	if b, ok := o.Tiers[ServiceBatch]; ok && !o.Pricing.IsZero() {
		mc.Pricing.BatchDiscount, mc.Pricing.BatchCachedInputPerMTok = b.Discount, b.CachedInputPerMTok
	}
	mc.ServerTools = slices.Clone(o.ServerTools)
	for _, mt := range o.Modalities.MediaTypes() {
		mc.Media.NativeTypes[mt] = true
	}
	mc.Notes = append(slices.Clone(o.Model.Notes), o.Notes...)
	off := o.Clone()
	mc.Offering = &off
	return mc
}

func intOf(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// OfferingFromCapabilities states a capability declaration as an
// offering: features, parameters with their ranges, the constraints the
// reasoning and Chat Completions rules imply, and the media types as
// input modalities. Capabilities on the result returns mc again (with
// Offering set). It is how a row registered as capabilities, rather than
// read from a catalog file, gets an offering.
//
//nolint:gocyclo // one flat mapping per capability field, the inverse of Capabilities
func OfferingFromCapabilities(mc ModelCapabilities) Offering {
	o := Offering{
		Model: ModelInfo{Vendor: ProviderName(mc.Provider), Prefix: ModelID(mc.Family),
			ContextWindow: mc.ContextWindow, MaxOutputTokens: mc.MaxOutputTokens, Known: mc.Known},
		StructuredOutput: mc.StructuredOutput,
		ServerTools:      slices.Clone(mc.ServerTools),
		DialMap:          mc.DialMap.Clone(),
		Notes:            slices.Clone(mc.Notes),
		Params:           ParamSpace{Params: map[ParamName]ParamSpec{}},
	}
	for _, c := range mc.List() {
		if IsFeature(c) {
			o.Features = append(o.Features, c)
		}
	}
	yes, no := true, false
	declare := func(p ParamName, extra bool, spec ParamSpec) {
		c, _ := ParamCapability(p)
		if !mc.Supports(c) && !extra {
			return
		}
		spec.Allowed = &no
		if mc.Supports(c) {
			spec.Allowed = &yes
		}
		o.Params.Params[p] = spec
	}
	for _, k := range knobs {
		switch k.param {
		case ParamMaxOutputTokens:
			spec := StandardParam(k.param)
			if mc.DefaultMaxOutputTokens != 0 {
				spec.Default = mc.DefaultMaxOutputTokens
			}
			declare(k.param, mc.DefaultMaxOutputTokens != 0, spec)
		case ParamToolChoice:
			spec := StandardParam(k.param)
			if mc.RejectsForcedToolChoice {
				spec.Values = []string{string(ToolChoiceNone)}
			}
			declare(k.param, mc.RejectsForcedToolChoice, spec)
		case ParamReasoningEffort:
			spec := StandardParam(k.param)
			spec.Values, spec.Required = slices.Clone(mc.ReasoningEfforts), mc.ReasoningRequired
			if mc.DefaultReasoningEffort != "" {
				spec.Default = mc.DefaultReasoningEffort
			}
			declare(k.param, mc.ReasoningEfforts != nil || mc.DefaultReasoningEffort != "" || mc.ReasoningRequired, spec)
		case ParamReasoningBudget:
			spec := StandardParam(k.param)
			spec.Required = mc.ReasoningRequired
			if mc.MinReasoningBudget != 0 {
				spec.Min = f64p(float64(mc.MinReasoningBudget))
			}
			if mc.MaxReasoningBudget != 0 {
				spec.Max = f64p(float64(mc.MaxReasoningBudget))
			}
			if mc.DynamicReasoningBudget || mc.ZeroReasoningBudget {
				spec.Special = map[string]string{}
				if mc.DynamicReasoningBudget {
					spec.Special["-1"] = SpecialDynamic
				}
				if mc.ZeroReasoningBudget {
					spec.Special["0"] = SpecialOff
				}
			}
			declare(k.param, mc.MinReasoningBudget != 0 || mc.MaxReasoningBudget != 0 || spec.Special != nil || mc.ReasoningRequired, spec)
		case ParamReasoningEnabled:
			spec := StandardParam(k.param)
			spec.Required = mc.ReasoningRequired
			if mc.ReasoningDefaultEnabled {
				spec.Default = true
			}
			declare(k.param, mc.ReasoningDefaultEnabled || mc.ReasoningRequired, spec)
		default:
			declare(k.param, false, StandardParam(k.param))
		}
	}
	keys := []string{ConstraintReasoningExclusive}
	cons := map[string]Constraint{ConstraintReasoningExclusive: ReasoningExclusive()}
	if len(mc.SamplingRequiresNoReasoning) > 0 {
		var forbid []ParamName
		for _, c := range mc.SamplingRequiresNoReasoning {
			p, ok := CapabilityParam(c)
			if !ok {
				p = ParamName(c)
			}
			forbid = append(forbid, p)
		}
		for k, c := range SamplingConstraints(forbid) {
			keys, cons[k] = append(keys, k), c
		}
	}
	if c, ok := ChatToolsConstraint(mc.ChatCompletionsTools); ok {
		keys, cons[ConstraintChatTools] = append(keys, ConstraintChatTools), c
	}
	slices.Sort(keys)
	for _, k := range keys {
		o.Params.Constraints = append(o.Params.Constraints, cons[k])
	}
	o.Pricing = mc.Pricing
	o.Pricing.BatchDiscount, o.Pricing.BatchCachedInputPerMTok = 0, 0
	o.Pricing.Modal, o.ModalityPricing = nil, maps.Clone(mc.Pricing.Modal)
	if !mc.Pricing.IsZero() && (mc.Pricing.BatchDiscount != 0 || mc.Pricing.BatchCachedInputPerMTok != 0) {
		o.Tiers = map[ServiceTier]TierSpec{ServiceBatch: {Transport: TransportBatch,
			Discount: mc.Pricing.BatchDiscount, CachedInputPerMTok: mc.Pricing.BatchCachedInputPerMTok}}
	}
	o.Model.In = []Modality{ModalityText}
	if !mc.Supports(CapEmbeddings) {
		o.Model.Out = []Modality{ModalityText}
	}
	for mt, ok := range mc.Media.NativeTypes {
		if !ok {
			continue
		}
		if o.Modalities.In == nil {
			o.Modalities.In = map[Modality]ModalityLimit{}
		}
		m := mt.Modality()
		l := o.Modalities.In[m]
		l.Media = append(l.Media, mt)
		slices.Sort(l.Media)
		o.Modalities.In[m] = l
		if !slices.Contains(o.Model.In, m) {
			o.Model.In = append(o.Model.In, m)
		}
	}
	sortModalities(o.Model.In)
	return o
}

// modalityOrder is the order modality lists are written in.
var modalityOrder = []Modality{ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ModalityDocument, ModalityFile}

// KnownModalities lists every Modality this SDK defines.
func KnownModalities() []Modality { return slices.Clone(modalityOrder) }

func sortModalities(m []Modality) {
	slices.SortStableFunc(m, func(a, b Modality) int {
		return slices.Index(modalityOrder, a) - slices.Index(modalityOrder, b)
	})
}

// StandardParam is the specification a parameter has unless an offering
// narrows it: its type and the range request validation has always
// applied.
func StandardParam(p ParamName) ParamSpec {
	switch p {
	case ParamTemperature:
		return ParamSpec{Type: ParamTypeNumber, Min: f64p(0), Max: f64p(2)}
	case ParamTopP:
		return ParamSpec{Type: ParamTypeNumber, Min: f64p(0), Max: f64p(1)}
	case ParamTopK:
		return ParamSpec{Type: ParamTypeNumber, Min: f64p(0)}
	case ParamFrequencyPenalty, ParamPresencePenalty:
		return ParamSpec{Type: ParamTypeNumber, Min: f64p(-2), Max: f64p(2)}
	case ParamSeed:
		return ParamSpec{Type: ParamTypeInteger}
	case ParamMaxOutputTokens:
		return ParamSpec{Type: ParamTypeInteger, Min: f64p(1)}
	case ParamStop:
		return ParamSpec{Type: ParamTypeStringList}
	case ParamParallelTools, ParamReasoningEnabled:
		return ParamSpec{Type: ParamTypeBoolean}
	case ParamToolChoice:
		return ParamSpec{Type: ParamTypeEnum, Values: []string{string(ToolChoiceNone), string(ToolChoiceRequired), string(ToolChoiceNamed)}}
	case ParamReasoningEffort, ParamPromptCacheRetention, ParamServiceTier:
		return ParamSpec{Type: ParamTypeEnum}
	case ParamReasoningBudget:
		return ParamSpec{Type: ParamTypeInteger}
	}
	return ParamSpec{}
}

// Space returns the parameter space request validation checks, with the
// model's output limit as the max_output_tokens maximum when the
// parameter declares none.
func (o Offering) Space() ParamSpace {
	s := o.Params.Clone()
	if spec, ok := s.Params[ParamMaxOutputTokens]; ok && spec.Max == nil && o.Model.MaxOutputTokens > 0 {
		spec.Max = f64p(float64(o.Model.MaxOutputTokens))
		s.Params[ParamMaxOutputTokens] = spec
	}
	return s
}

// Clone returns a deep copy of the space.
func (s ParamSpace) Clone() ParamSpace {
	out := ParamSpace{}
	if s.Params != nil {
		out.Params = make(map[ParamName]ParamSpec, len(s.Params))
		for k, v := range s.Params {
			out.Params[k] = v.Clone()
		}
	}
	for _, c := range s.Constraints {
		out.Constraints = append(out.Constraints, c.Clone())
	}
	return out
}

// Clone returns a deep copy of the spec.
func (s ParamSpec) Clone() ParamSpec {
	if s.Min != nil {
		s.Min = f64p(*s.Min)
	}
	if s.Max != nil {
		s.Max = f64p(*s.Max)
	}
	if s.Allowed != nil {
		s.Allowed = boolp(*s.Allowed)
	}
	s.Values = slices.Clone(s.Values)
	s.Special = maps.Clone(s.Special)
	return s
}

// Clone returns a deep copy of the constraint.
func (c Constraint) Clone() Constraint {
	out := Constraint{Forbid: slices.Clone(c.Forbid), Exclusive: slices.Clone(c.Exclusive), Reason: c.Reason}
	if c.When != nil {
		out.When = Condition{}
		for k, v := range c.When {
			out.When[k] = v.clone()
		}
	}
	if c.Require != nil {
		out.Require = map[ParamName][]string{}
		for k, v := range c.Require {
			out.Require[k] = slices.Clone(v)
		}
	}
	return out
}

func (v CondValue) clone() CondValue {
	out := CondValue{In: slices.Clone(v.In), Not: slices.Clone(v.Not)}
	if v.Bool != nil {
		out.Bool = boolp(*v.Bool)
	}
	if v.Set != nil {
		out.Set = boolp(*v.Set)
	}
	return out
}

func (l ModalityLimit) clone() ModalityLimit {
	l.Media = slices.Clone(l.Media)
	l.Sources = slices.Clone(l.Sources)
	if l.FPS != nil {
		f := l.FPS.Clone()
		l.FPS = &f
	}
	return l
}

func (m Modalities) clone() Modalities {
	out := Modalities{ToolResult: maps.Clone(m.ToolResult)}
	if m.In != nil {
		out.In = map[Modality]ModalityLimit{}
		for k, v := range m.In {
			out.In[k] = v.clone()
		}
	}
	if m.Out != nil {
		out.Out = map[Modality]ModalityLimit{}
		for k, v := range m.Out {
			out.Out[k] = v.clone()
		}
	}
	return out
}

// Clone returns a deep copy of the offering.
func (o Offering) Clone() Offering {
	out := o
	out.Model.In, out.Model.Out = slices.Clone(o.Model.In), slices.Clone(o.Model.Out)
	out.Model.Notes = slices.Clone(o.Model.Notes)
	if o.Endpoint.Data.Store != nil {
		out.Endpoint.Data.Store = boolp(*o.Endpoint.Data.Store)
	}
	out.Endpoint.Files.URISchemes = slices.Clone(o.Endpoint.Files.URISchemes)
	out.Features = slices.Clone(o.Features)
	out.Params = o.Params.Clone()
	out.Modalities = o.Modalities.clone()
	out.ServerTools = slices.Clone(o.ServerTools)
	out.ServerToolFees = maps.Clone(o.ServerToolFees)
	if o.Tiers != nil {
		out.Tiers = make(map[ServiceTier]TierSpec, len(o.Tiers))
		for k, v := range o.Tiers {
			if v.Pricing != nil {
				p := *v.Pricing
				v.Pricing = &p
			}
			out.Tiers[k] = v
		}
	}
	out.ModalityPricing = maps.Clone(o.ModalityPricing)
	out.Defaults = o.Defaults.Clone()
	out.DialMap = o.DialMap.Clone()
	out.Fallback = FallbackHints{Equivalents: slices.Clone(o.Fallback.Equivalents), LargerContext: slices.Clone(o.Fallback.LargerContext)}
	out.Notes = slices.Clone(o.Notes)
	return out
}

// Intersect returns what a caller may rely on when either offering might
// serve a request, as ModelCapabilities.Intersect does for the projection:
// parameter spaces intersect (ranges narrow, enums intersect, every
// constraint of both holds), input modalities intersect with each limit
// taking the smaller declared value, tool-result lowering takes the
// stricter rule, features and server tools intersect, and pricing takes
// the costlier rate of each line.
func (o Offering) Intersect(other Offering) Offering {
	out := Offering{
		ID:               o.ID + "&" + other.ID,
		Model:            o.Model,
		Endpoint:         EndpointInfo{Name: o.Endpoint.Name + "&" + other.Endpoint.Name},
		Params:           o.Params.Intersect(other.Params),
		StructuredOutput: weakerStructuredOutput(o.StructuredOutput, other.StructuredOutput),
		Pricing:          worsePricing(o.Pricing, other.Pricing),
		Notes:            append(slices.Clone(o.Notes), other.Notes...),
	}
	out.ModalityPricing = worsePricing(Pricing{InputPerMTok: 1, Modal: o.modal()}, Pricing{InputPerMTok: 1, Modal: other.modal()}).Modal
	out.Model.Known = o.Model.Known && other.Model.Known
	out.Model.ContextWindow = minNonZero(o.Model.ContextWindow, other.Model.ContextWindow)
	out.Model.MaxOutputTokens = minNonZero(o.Model.MaxOutputTokens, other.Model.MaxOutputTokens)
	out.Model.In = intersectList(o.Model.In, other.Model.In)
	out.Model.Out = intersectList(o.Model.Out, other.Model.Out)
	if o.Model.Vendor != other.Model.Vendor {
		out.Model.Vendor = o.Model.Vendor + "+" + other.Model.Vendor
		out.Model.Prefix = ""
	}
	out.Features = intersectList(o.Features, other.Features)
	out.ServerTools = intersectList(o.ServerTools, other.ServerTools)
	out.Modalities.In = intersectLimits(o.Modalities.In, other.Modalities.In)
	out.Modalities.Out = intersectLimits(o.Modalities.Out, other.Modalities.Out)
	for m, a := range o.Modalities.ToolResult {
		if b, ok := other.Modalities.ToolResult[m]; ok {
			if out.Modalities.ToolResult == nil {
				out.Modalities.ToolResult = map[Modality]string{}
			}
			out.Modalities.ToolResult[m] = stricterLowering(a, b)
		}
	}
	for t, a := range o.Tiers {
		if b, ok := other.Tiers[t]; ok && a.Transport == b.Transport {
			if out.Tiers == nil {
				out.Tiers = map[ServiceTier]TierSpec{}
			}
			out.Tiers[t] = TierSpec{Transport: a.Transport, Discount: min(a.Discount, b.Discount),
				CachedInputPerMTok: declaredMax(a.CachedInputPerMTok, b.CachedInputPerMTok)}
		}
	}
	return out
}

func intersectList[T comparable](a, b []T) []T {
	var out []T
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func intersectLimits(a, b map[Modality]ModalityLimit) map[Modality]ModalityLimit {
	var out map[Modality]ModalityLimit
	for m, la := range a {
		lb, ok := b[m]
		if !ok {
			continue
		}
		l := ModalityLimit{
			Media:       intersectList(la.Media, lb.Media),
			Sources:     intersectList(la.Sources, lb.Sources),
			MaxBytes:    minNonZero64(la.MaxBytes, lb.MaxBytes),
			MaxCount:    minNonZero(la.MaxCount, lb.MaxCount),
			MaxPixels:   minNonZero(la.MaxPixels, lb.MaxPixels),
			MaxPages:    minNonZero(la.MaxPages, lb.MaxPages),
			MaxDuration: time.Duration(minNonZero64(int64(la.MaxDuration), int64(lb.MaxDuration))),
		}
		if la.FPS != nil && lb.FPS != nil {
			f := la.FPS.intersect(*lb.FPS)
			l.FPS = &f
		}
		// The costlier token rule is the safe estimate.
		if lb.Tokens.Base+lb.Tokens.PerTile+lb.Tokens.PerPage+lb.Tokens.PerSecond+lb.Tokens.PerImage >
			la.Tokens.Base+la.Tokens.PerTile+la.Tokens.PerPage+la.Tokens.PerSecond+la.Tokens.PerImage {
			l.Tokens = lb.Tokens
		} else {
			l.Tokens = la.Tokens
		}
		if out == nil {
			out = map[Modality]ModalityLimit{}
		}
		out[m] = l
	}
	return out
}

func minNonZero64(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// stricterLowering ranks tool-result lowering: none is strictest, then a
// follow-up user message, then inline.
func stricterLowering(a, b string) string {
	rank := map[string]int{ToolResultInline: 0, ToolResultFollowUpUser: 1, ToolResultNone: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
