package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Dials are model-neutral generation intents. Raw RequestOptions name a
// vendor parameter and are rejected when a model cannot take it (D-12). A
// dial names what the caller wants, and ResolveDials compiles it per attempt
// against the model that serves it: an advisory dial the model cannot honor
// is mapped to the nearest declared value or dropped, a contractual dial is
// rejected, and every decision is recorded in a DialReport.
//
// Nil fields are unset. Dials merge field by field, except Reasoning, whose
// mode and depth merge together.
type Dials struct {
	// Creativity compiles to temperature, top_p and top_k. Advisory.
	Creativity *Creativity `json:"creativity,omitempty"`
	// Reasoning compiles to a reasoning toggle, effort, budget or level.
	// Advisory: the nearest declared value is sent.
	Reasoning *ReasoningDial `json:"reasoning,omitempty"`
	// MaxOutput caps output tokens. It is never raised; a value above the
	// model's ceiling is lowered to it and recorded.
	MaxOutput *int64 `json:"max_output,omitempty"`
	// Tools compiles to the tool choice. Contractual (D-24).
	Tools *ToolChoice `json:"tools,omitempty"`
	// Parallel compiles to parallel tool control. False is contractual,
	// because the tools may be unsafe to run concurrently; true is advisory.
	Parallel *bool `json:"parallel,omitempty"`
	// Seed asks for reproducible sampling. Contractual. A seed is not a
	// guarantee on any vendor; the response cache is what replays a call.
	Seed *int64 `json:"reproducible,omitempty"`
	// Cache turns provider-side prompt caching on or off. Advisory, cost
	// only. It selects the prompt cache mode the catalog row declares, so it
	// takes effect only in the layers a provider is built from.
	Cache *bool `json:"cache,omitempty"`
}

// Creativity is a sampling intent.
type Creativity string

const (
	CreativityDeterministic Creativity = "deterministic"
	CreativityFocused       Creativity = "focused"
	CreativityBalanced      Creativity = "balanced"
	CreativityCreative      Creativity = "creative"
)

// creativityOrder ranks the creativity levels for nearest-value mapping.
var creativityOrder = []Creativity{CreativityDeterministic, CreativityFocused, CreativityBalanced, CreativityCreative}

// ReasoningMode says whether the model reasons.
type ReasoningMode string

const (
	ReasoningOff      ReasoningMode = "off"
	ReasoningAdaptive ReasoningMode = "adaptive" // the model decides how much
	ReasoningOn       ReasoningMode = "on"
)

// Depth is how much the model reasons when reasoning is on.
type Depth string

const (
	DepthMinimal Depth = "minimal"
	DepthLow     Depth = "low"
	DepthMedium  Depth = "medium"
	DepthHigh    Depth = "high"
	DepthMax     Depth = "max"
)

// depthOrder ranks the depths for nearest-value mapping.
var depthOrder = []Depth{DepthMinimal, DepthLow, DepthMedium, DepthHigh, DepthMax}

// ReasoningDial is the reasoning intent. A depth without a mode means on.
type ReasoningDial struct {
	Mode  ReasoningMode `json:"mode,omitempty"`
	Depth Depth         `json:"depth,omitempty"`
}

// normalized fills the implied mode.
func (r ReasoningDial) normalized() ReasoningDial {
	if r.Mode == "" {
		r.Mode = ReasoningOn
	}
	if r.Mode == ReasoningOff {
		r.Depth = ""
	}
	return r
}

// String renders the dial as "off", "on", "adaptive" or "on:high".
func (r ReasoningDial) String() string {
	r = r.normalized()
	if r.Depth == "" {
		return string(r.Mode)
	}
	return string(r.Mode) + ":" + string(r.Depth)
}

// DialName names one dial.
type DialName string

const (
	DialCreativity   DialName = "creativity"
	DialReasoning    DialName = "reasoning"
	DialMaxOutput    DialName = "max_output"
	DialTools        DialName = "tools"
	DialParallel     DialName = "parallel"
	DialReproducible DialName = "reproducible"
	DialCache        DialName = "cache"
)

// AllDialNames lists every dial in canonical order.
func AllDialNames() []DialName {
	return []DialName{DialCreativity, DialReasoning, DialMaxOutput, DialTools, DialParallel, DialReproducible, DialCache}
}

// DialClass says how a dial is handled by default when the model cannot
// honor it.
type DialClass string

const (
	// DialAdvisory: mapped to the nearest declared value, or dropped.
	DialAdvisory DialClass = "advisory"
	// DialClamp: lowered to the model's limit, never raised.
	DialClamp DialClass = "clamp"
	// DialContractual: rejected with ErrInvalidModelConfig.
	DialContractual DialClass = "contractual"
)

// Class returns the dial's class for the value in d. Parallel is
// contractual only when it is false.
func (n DialName) Class(d Dials) DialClass {
	switch n {
	case DialMaxOutput:
		return DialClamp
	case DialTools, DialReproducible:
		return DialContractual
	case DialParallel:
		if d.Parallel != nil && !*d.Parallel {
			return DialContractual
		}
	}
	return DialAdvisory
}

// IsZero reports whether no dial is set.
func (d Dials) IsZero() bool {
	return d.Creativity == nil && d.Reasoning == nil && d.MaxOutput == nil && d.Tools == nil &&
		d.Parallel == nil && d.Seed == nil && d.Cache == nil
}

// Names returns the names of the dials set in d, in canonical order.
func (d Dials) Names() []DialName {
	set := map[DialName]bool{
		DialCreativity: d.Creativity != nil, DialReasoning: d.Reasoning != nil, DialMaxOutput: d.MaxOutput != nil,
		DialTools: d.Tools != nil, DialParallel: d.Parallel != nil, DialReproducible: d.Seed != nil, DialCache: d.Cache != nil,
	}
	var out []DialName
	for _, n := range AllDialNames() {
		if set[n] {
			out = append(out, n)
		}
	}
	return out
}

// Merge returns d with every dial set in over replacing d's, field by field.
// Reasoning replaces whole. Neither input is modified.
func (d Dials) Merge(over Dials) Dials {
	out, over := d.Clone(), over.Clone()
	if over.Creativity != nil {
		out.Creativity = over.Creativity
	}
	if over.Reasoning != nil {
		out.Reasoning = over.Reasoning
	}
	if over.MaxOutput != nil {
		out.MaxOutput = over.MaxOutput
	}
	if over.Tools != nil {
		out.Tools = over.Tools
	}
	if over.Parallel != nil {
		out.Parallel = over.Parallel
	}
	if over.Seed != nil {
		out.Seed = over.Seed
	}
	if over.Cache != nil {
		out.Cache = over.Cache
	}
	return out
}

// Without returns a copy of d with the named dials cleared.
func (d Dials) Without(names ...DialName) Dials {
	out := d.Clone()
	for _, n := range names {
		switch n {
		case DialCreativity:
			out.Creativity = nil
		case DialReasoning:
			out.Reasoning = nil
		case DialMaxOutput:
			out.MaxOutput = nil
		case DialTools:
			out.Tools = nil
		case DialParallel:
			out.Parallel = nil
		case DialReproducible:
			out.Seed = nil
		case DialCache:
			out.Cache = nil
		}
	}
	return out
}

// Clone returns a deep copy.
func (d Dials) Clone() Dials {
	return Dials{Creativity: clonePtr(d.Creativity), Reasoning: clonePtr(d.Reasoning), MaxOutput: clonePtr(d.MaxOutput),
		Tools: clonePtr(d.Tools), Parallel: clonePtr(d.Parallel), Seed: clonePtr(d.Seed), Cache: clonePtr(d.Cache)}
}

// Validate checks the dial values themselves, independently of any model.
// An unknown value is an error whatever the policy, since no model can
// honor it.
func (d Dials) Validate() error {
	bad := func(name DialName, format string, args ...any) error {
		return fmt.Errorf("%w: dial %s: %s", ErrInvalidModelConfig, name, fmt.Sprintf(format, args...))
	}
	if d.Creativity != nil && !slices.Contains(creativityOrder, *d.Creativity) {
		return bad(DialCreativity, "unknown value %q", *d.Creativity)
	}
	if r := d.Reasoning; r != nil {
		switch r.Mode {
		case "", ReasoningOff, ReasoningAdaptive, ReasoningOn:
		default:
			return bad(DialReasoning, "unknown mode %q", r.Mode)
		}
		if r.Depth != "" && !slices.Contains(depthOrder, r.Depth) {
			return bad(DialReasoning, "unknown depth %q", r.Depth)
		}
		if r.Mode == ReasoningOff && r.Depth != "" {
			return bad(DialReasoning, "a depth needs reasoning on")
		}
		if r.Mode == "" && r.Depth == "" {
			return bad(DialReasoning, "set a mode or a depth")
		}
	}
	if d.MaxOutput != nil && *d.MaxOutput <= 0 {
		return bad(DialMaxOutput, "must be positive")
	}
	if d.Tools != nil {
		if err := d.Tools.Validate(nil); err != nil {
			return err
		}
	}
	return nil
}

// Handling is what happens to a dial the model cannot honor exactly.
type Handling string

const (
	// HandlingReject fails the attempt with ErrInvalidModelConfig.
	HandlingReject Handling = "reject"
	// HandlingNearest sends the nearest declared value, or drops the dial
	// when there is none.
	HandlingNearest Handling = "nearest"
	// HandlingDrop sends nothing for the dial.
	HandlingDrop Handling = "drop"
)

// DialPolicy overrides the default handling of dials. The zero value uses
// each dial's class: advisory and clamp dials take the nearest value, and
// contractual dials reject.
type DialPolicy struct {
	// Strict rejects every dial a model cannot honor exactly. It takes
	// precedence over Per. Evals use it to hold settings constant.
	Strict bool `json:"strict,omitempty"`
	// Per overrides one dial. Naming a contractual dial here is the only
	// way to loosen it; there is no global lenient switch.
	Per map[DialName]Handling `json:"per,omitempty"`
}

// StrictDials rejects every dial a model cannot honor exactly.
var StrictDials = DialPolicy{Strict: true}

// Handling returns the handling of one dial for the value in d.
func (p DialPolicy) Handling(name DialName, d Dials) Handling {
	if p.Strict {
		return HandlingReject
	}
	if h, ok := p.Per[name]; ok && h != "" {
		return h
	}
	if name.Class(d) == DialContractual {
		return HandlingReject
	}
	return HandlingNearest
}

// Validate checks the policy's names and handlings.
func (p DialPolicy) Validate() error {
	for n, h := range p.Per {
		if !slices.Contains(AllDialNames(), n) {
			return fmt.Errorf("%w: dial policy: unknown dial %q", ErrInvalidModelConfig, n)
		}
		switch h {
		case HandlingReject, HandlingNearest, HandlingDrop:
		default:
			return fmt.Errorf("%w: dial policy: unknown handling %q for %s", ErrInvalidModelConfig, h, n)
		}
	}
	return nil
}

// String renders the policy for spans and reports: "default", "strict", or
// "default" followed by each override as "name=handling".
func (p DialPolicy) String() string {
	if p.Strict {
		return "strict"
	}
	if len(p.Per) == 0 {
		return "default"
	}
	parts := []string{"default"}
	for _, n := range slices.Sorted(maps.Keys(p.Per)) {
		parts = append(parts, string(n)+"="+string(p.Per[n]))
	}
	return strings.Join(parts, " ")
}

// Clone returns a deep copy.
func (p DialPolicy) Clone() DialPolicy {
	return DialPolicy{Strict: p.Strict, Per: maps.Clone(p.Per)}
}

// Dial scopes, lowest precedence first. They name the layer that set a dial
// in a DialDecision.
const (
	DialScopeGlobal  = "global"
	DialScopeModel   = "model"
	DialScopePreset  = "preset"
	DialScopeEntry   = "entry"
	DialScopeAgent   = "agent"
	DialScopeMember  = "member"
	DialScopeTurn    = "turn"
	DialScopeRequest = "request"
)

// buildScope reports whether a scope is one a provider is built from.
func buildScope(scope string) bool {
	switch scope {
	case DialScopeGlobal, DialScopeModel, DialScopePreset, DialScopeEntry:
		return true
	}
	return false
}

// DialLayer is one source of dials, applied in order: a later layer
// overrides an earlier one field by field.
type DialLayer struct {
	Scope string `json:"scope"`
	Dials Dials  `json:"dials"`
	// Hold is the reasoning in force when an open tool loop with signed
	// reasoning started. A different reasoning in this layer waits for the
	// next user turn: a mode change always, a depth change unless the model
	// declares that depth may change per request. The agent sets it.
	Hold *ReasoningDial `json:"hold,omitempty"`
}

// Clone returns a deep copy.
func (l DialLayer) Clone() DialLayer {
	return DialLayer{Scope: l.Scope, Dials: l.Dials.Clone(), Hold: clonePtr(l.Hold)}
}

func cloneLayers(in []DialLayer) []DialLayer {
	if in == nil {
		return nil
	}
	out := make([]DialLayer, len(in))
	for i, l := range in {
		out[i] = l.Clone()
	}
	return out
}

// DialContext describes the request a dial is compiled for.
type DialContext struct {
	// Tools and Schema say whether the request offers tools or a response
	// schema.
	Tools, Schema bool
	// Surface names the vendor API that serves the request, for rows whose
	// mapping depends on it, such as SurfaceChat.
	Surface string
	// Previous is the effective options of the previous call in the same
	// conversation, when known. A reasoning change against it is reported
	// as CacheResetExpected on a row that declares one.
	Previous *RequestOptions
}

// API surfaces reported by DialSurfaceReporter.
const (
	SurfaceChat      = "chat"
	SurfaceResponses = "responses"
)

// DialSurfaceReporter is an optional interface for an adapter whose vendor
// offers more than one API for the same model, so dials can compile for
// the one in use. Decorators need not forward it; callers look for it on
// the innermost provider.
type DialSurfaceReporter interface {
	DialSurface() string
}

// DialAction is what happened to one dial.
type DialAction string

const (
	DialApplied DialAction = "applied"
	// DialMapped: a different value than requested was sent, the nearest the
	// model declares, or the requested value lowered to a limit.
	DialMapped DialAction = "mapped"
	// DialDropped: nothing was sent for the dial.
	DialDropped DialAction = "dropped"
	// DialRejected: the attempt failed with ErrInvalidModelConfig.
	DialRejected DialAction = "rejected"
	// DialRawOverride: a raw option set the same parameter and won.
	DialRawOverride DialAction = "raw_override"
	// DialDeferred: the change waits for the next user turn.
	DialDeferred DialAction = "deferred"
)

// DialDecision records how one dial was compiled for one attempt.
type DialDecision struct {
	Dial DialName `json:"dial"`
	// Requested is the dial value, and Sent the raw parameters it compiled
	// to, or empty when nothing was sent.
	Requested string     `json:"requested"`
	Sent      string     `json:"sent,omitempty"`
	Action    DialAction `json:"action"`
	// Reason explains a decision other than applied, such as "conflicts
	// with effort medium".
	Reason string `json:"reason,omitempty"`
	// Scope is the layer that set the dial.
	Scope string `json:"scope,omitempty"`
	// CacheResetExpected reports that the change invalidates the
	// provider's cached prompt prefix.
	CacheResetExpected bool `json:"cache_reset_expected,omitempty"`
}

// String renders the decision as "name:requested→sent" for a mapped dial
// and "name:requested" otherwise.
func (d DialDecision) String() string {
	if d.Action == DialMapped || d.Action == DialRawOverride || d.Action == DialDeferred {
		return string(d.Dial) + ":" + d.Requested + "→" + d.Sent
	}
	return string(d.Dial) + ":" + d.Requested
}

// DialReport records how the dials of one attempt were compiled.
type DialReport struct {
	// Requested is the merged dials.
	Requested Dials
	// Effective is the raw options the adapter sends.
	Effective RequestOptions
	// EffectiveHash is the first 16 hex digits of a SHA-256 over Effective,
	// so two attempts that sent the same parameters compare equal.
	EffectiveHash string
	Decisions     []DialDecision
	// Policy is the policy in force, as DialPolicy.String renders it.
	Policy string
}

// Changed reports whether any dial was mapped, dropped, overridden or
// deferred, which is when a report is worth a note in the tree.
func (r DialReport) Changed() bool {
	return slices.ContainsFunc(r.Decisions, func(d DialDecision) bool { return d.Action != DialApplied })
}

// Decision returns the decision for one dial.
func (r DialReport) Decision(name DialName) (DialDecision, bool) {
	i := slices.IndexFunc(r.Decisions, func(d DialDecision) bool { return d.Dial == name })
	if i < 0 {
		return DialDecision{}, false
	}
	return r.Decisions[i], true
}

// Lines renders the decisions with the given action, in order.
func (r DialReport) Lines(action DialAction) []string {
	var out []string
	for _, d := range r.Decisions {
		if d.Action == action {
			out = append(out, d.String())
		}
	}
	return out
}

// Clone returns a deep copy.
func (r DialReport) Clone() DialReport {
	out := r
	out.Requested = r.Requested.Clone()
	out.Effective = r.Effective.Clone()
	out.Decisions = slices.Clone(r.Decisions)
	return out
}

type wireDialReport struct {
	Requested     Dials          `json:"requested"`
	Effective     *wireOptions   `json:"effective,omitempty"`
	EffectiveHash string         `json:"effective_hash,omitempty"`
	Decisions     []DialDecision `json:"decisions,omitempty"`
	Policy        string         `json:"policy,omitempty"`
}

// MarshalJSON writes the report with Effective in the snake_case wire form.
func (r DialReport) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireDialReport{Requested: r.Requested, Effective: toWireOptions(&r.Effective),
		EffectiveHash: r.EffectiveHash, Decisions: r.Decisions, Policy: r.Policy})
}

// UnmarshalJSON reads a report written by MarshalJSON.
func (r *DialReport) UnmarshalJSON(data []byte) error {
	var w wireDialReport
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*r = DialReport{Requested: w.Requested, EffectiveHash: w.EffectiveHash, Decisions: w.Decisions, Policy: w.Policy}
	if o := w.Effective.requestOptions(); o != nil {
		r.Effective = *o
	}
	return nil
}

// optionsHash hashes the raw parameters of o.
func optionsHash(o RequestOptions) string {
	data, _ := json.Marshal(toWireOptions(&o))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// HasDials reports whether o carries dials to compile.
func (o RequestOptions) HasDials() bool {
	if !o.Dials.IsZero() {
		return true
	}
	return slices.ContainsFunc(o.DialLayers, func(l DialLayer) bool { return !l.Dials.IsZero() })
}

// Layers returns o's dial layers in precedence order: DialLayers, then
// Dials at request scope.
func (o RequestOptions) Layers() []DialLayer {
	out := cloneLayers(o.DialLayers)
	if !o.Dials.IsZero() {
		out = append(out, DialLayer{Scope: DialScopeRequest, Dials: o.Dials.Clone()})
	}
	return out
}

// Raw returns a copy of o without its dials and dial policy.
func (o RequestOptions) Raw() RequestOptions {
	out := o.Clone()
	out.Dials, out.DialLayers, out.DialPolicy = Dials{}, nil, nil
	return out
}

// CompileOptions compiles the dials o carries against mc with
// ResolveDials, using o's dial policy. The result carries no dials. When o
// has none it is returned as is, with a nil report.
func CompileOptions(mc ModelCapabilities, o RequestOptions, ctx DialContext) (RequestOptions, *DialReport, error) {
	if !o.HasDials() {
		return o.Raw(), nil, nil
	}
	var pol DialPolicy
	if o.DialPolicy != nil {
		pol = *o.DialPolicy
	}
	eff, rep, err := ResolveDials(mc, o.Raw(), ctx, pol, o.Layers()...)
	if err != nil {
		return RequestOptions{}, &rep, err
	}
	return eff, &rep, nil
}
