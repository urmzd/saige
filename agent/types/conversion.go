package types

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// Media and modality errors. Both match ErrInvalidModelConfig: the request
// cannot be sent as it is, and retrying it unchanged cannot help.
var (
	// ErrMediaUnavailable reports a media part with no locator a provider
	// can use, such as bytes that were never persisted.
	ErrMediaUnavailable error = &invalidConfigError{msg: "media unavailable"}
	// ErrModalityUnsupported reports a part the serving model cannot take
	// natively and that no permitted conversion could turn into a part it
	// can take.
	ErrModalityUnsupported error = &invalidConfigError{msg: "modality not supported"}
)

// ModalityAction is what may be done with a part a model cannot take
// natively.
type ModalityAction string

const (
	// ActReject refuses the request. It is the default.
	ActReject ModalityAction = "reject"
	// ActConvert keeps the modality and changes the format or locator: webp
	// to png, wav to mp3, bytes to a vendor upload, a URI to inline bytes.
	ActConvert ModalityAction = "convert"
	// ActTranscribe turns audio, or a video's audio track, into text.
	ActTranscribe ModalityAction = "transcribe"
	// ActDescribe turns an image or video into text through a vision model.
	ActDescribe ModalityAction = "describe"
	// ActExtract turns a document into text, or a video into image frames.
	ActExtract ModalityAction = "extract"
	// ActOmit drops the part and records a notice in its place.
	ActOmit ModalityAction = "omit"
)

// ModalityDial is the "modality" dial: which actions are permitted per
// modality, in order of preference. Default applies to a modality Per does
// not name.
type ModalityDial struct {
	Default ModalityAction                `json:"default,omitempty"`
	Per     map[Modality][]ModalityAction `json:"per,omitempty"`
}

// Actions returns the permitted actions for m, in preference order. With
// nothing declared it is ActReject alone.
func (d ModalityDial) Actions(m Modality) []ModalityAction {
	if as, ok := d.Per[m]; ok && len(as) > 0 {
		return slices.Clone(as)
	}
	if d.Default != "" {
		return []ModalityAction{d.Default}
	}
	return []ModalityAction{ActReject}
}

// Merge returns d with over applied: a non-empty Default replaces d's, and
// each modality over names replaces d's list for it.
func (d ModalityDial) Merge(over ModalityDial) ModalityDial {
	out := ModalityDial{Default: d.Default}
	if over.Default != "" {
		out.Default = over.Default
	}
	if len(d.Per) > 0 || len(over.Per) > 0 {
		out.Per = make(map[Modality][]ModalityAction, len(d.Per)+len(over.Per))
		for m, as := range d.Per {
			out.Per[m] = slices.Clone(as)
		}
		for m, as := range over.Per {
			out.Per[m] = slices.Clone(as)
		}
	}
	return out
}

// Clone returns a deep copy of d.
func (d ModalityDial) Clone() ModalityDial {
	return ModalityDial{}.Merge(d)
}

// Validate checks every action d names, and that reject or omit is not
// followed by another action: neither can fail, so nothing after it runs.
func (d ModalityDial) Validate() error {
	check := func(where string, as []ModalityAction) error {
		for i, a := range as {
			if !slices.Contains(KnownModalityActions(), a) {
				return fmt.Errorf("%s: unknown action %q", where, a)
			}
			if (a == ActReject || a == ActOmit) && i < len(as)-1 {
				return fmt.Errorf("%s: %s must be the last action", where, a)
			}
		}
		return nil
	}
	if d.Default != "" {
		if err := check("default", []ModalityAction{d.Default}); err != nil {
			return err
		}
	}
	for m, as := range d.Per {
		if !slices.Contains(KnownModalities(), m) {
			return fmt.Errorf("unknown modality %q", m)
		}
		if err := check(string(m), as); err != nil {
			return err
		}
	}
	return nil
}

// KnownModalityActions lists every ModalityAction.
func KnownModalityActions() []ModalityAction {
	return []ModalityAction{ActReject, ActConvert, ActTranscribe, ActDescribe, ActExtract, ActOmit}
}

// PartPath locates a part in a request: the message, the part in it, and
// for a part inside a tool result, its position there (-1 otherwise).
type PartPath struct {
	Message int `json:"message"`
	Part    int `json:"part"`
	Nested  int `json:"nested"`
}

func (p PartPath) String() string {
	if p.Nested >= 0 {
		return fmt.Sprintf("%d.%d.%d", p.Message, p.Part, p.Nested)
	}
	return fmt.Sprintf("%d.%d", p.Message, p.Part)
}

// Conversion decision actions recorded in a report.
const (
	DecisionNative      = "native"
	DecisionLowered     = "lowered"
	DecisionConverted   = "converted"
	DecisionTranscribed = "transcribed"
	DecisionDescribed   = "described"
	DecisionExtracted   = "extracted"
	DecisionOmitted     = "omitted"
	DecisionRejected    = "rejected"
)

// ConversionDecision records what happened to one part on one attempt.
type ConversionDecision struct {
	Path      PartPath  `json:"path"`
	Kind      PartKind  `json:"kind"`
	MediaType MediaType `json:"media_type,omitempty"`
	Digest    string    `json:"sha256,omitempty"`
	// Action is one of the Decision constants.
	Action string `json:"action"`
	// Via names the converter (name@version) or the offering that did the
	// work.
	Via string `json:"via,omitempty"`
	// Produced are references to the derivative parts.
	Produced []string `json:"produced,omitempty"`
	Cached   bool     `json:"cached,omitempty"`
	Cost     *Cost    `json:"cost,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	// Scope names the dial layer that permitted the action.
	Scope string `json:"scope,omitempty"`
}

// ConversionReport is every decision one attempt made, for the offering it
// targeted. Hash identifies the report's content; a response cache keys on
// it so a converted view never replays as the original.
type ConversionReport struct {
	Offering  string               `json:"offering,omitempty"`
	Decisions []ConversionDecision `json:"decisions,omitempty"`
	Hash      string               `json:"hash,omitempty"`
}

// Clone returns a deep copy of r.
func (r ConversionReport) Clone() ConversionReport {
	out := ConversionReport{Offering: r.Offering, Hash: r.Hash}
	if r.Decisions != nil {
		out.Decisions = make([]ConversionDecision, len(r.Decisions))
		for i, d := range r.Decisions {
			d.Produced = slices.Clone(d.Produced)
			if d.Cost != nil {
				c := *d.Cost
				d.Cost = &c
			}
			out.Decisions[i] = d
		}
	}
	return out
}

// ComputeHash returns the hex SHA-256 of the report's offering and
// decisions, ignoring Hash itself, cost and cache hits, so the same view
// hashes the same whether or not it was served from a cache.
func (r ConversionReport) ComputeHash() string {
	type key struct {
		Path      PartPath  `json:"path"`
		Kind      PartKind  `json:"kind"`
		MediaType MediaType `json:"media_type"`
		Digest    string    `json:"sha256"`
		Action    string    `json:"action"`
		Via       string    `json:"via"`
	}
	keys := make([]key, len(r.Decisions))
	for i, d := range r.Decisions {
		keys[i] = key{d.Path, d.Kind, d.MediaType, d.Digest, d.Action, d.Via}
	}
	b, _ := json.Marshal(struct {
		Offering string `json:"offering"`
		Keys     []key  `json:"decisions"`
	}{r.Offering, keys})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ConversionEstimate is an upper bound on what a conversion will cost.
type ConversionEstimate struct {
	InputTokens  int  `json:"input_tokens,omitempty"`
	OutputTokens int  `json:"output_tokens,omitempty"`
	Cost         Cost `json:"cost,omitzero"`
}

// ConversionUsage is what a conversion consumed.
type ConversionUsage struct {
	Usage UsageDelta `json:"-"`
	Cost  Cost       `json:"cost,omitzero"`
	// Cached is true when the result came from a conversion cache.
	Cached bool `json:"cached,omitempty"`
	// Model and Pricing name what a converter that called a model was billed
	// as, so the conversion settles against the run's budget like any other
	// provider call. Both are empty for a converter that calls no model.
	Model   string  `json:"model,omitempty"`
	Pricing Pricing `json:"-"`
}

// Converter turns a part the serving offering cannot take into parts it
// can. A converter that calls a model (transcribe, describe) reports what
// the call used in ConversionUsage; the executor reserves its Estimate from
// the attempt's budget before the call and settles the usage after it.
//
// A converter's output must depend only on the part and its own version:
// results are memoized by the part's digest and Name@Version, and reused
// across turns and across the offerings a request fails over between.
type Converter interface {
	Name() string
	// Version changes whenever the output for the same input may change:
	// a new prompt, model or extraction library.
	Version() string
	Action() ModalityAction
	// Accepts reports whether the converter can convert p.
	Accepts(p Part) bool
	// Produces lists the modalities the converted parts have.
	Produces(p Part) []Modality
	// Estimate bounds what converting p costs. It must not do I/O.
	Estimate(p Part, target Offering) (ConversionEstimate, error)
	Convert(ctx context.Context, p Part, env ConvertEnv) ([]Part, ConversionUsage, error)
}

// TargetedConverter is implemented by a converter whose output fits only
// some offerings, such as a transcoder to an image format the target must
// accept. The planner skips it for a target it does not fit.
type TargetedConverter interface {
	Converter
	Fits(p Part, target Offering) bool
}

// ConvertEnv is what a converter is told about the attempt it converts for.
type ConvertEnv struct {
	// Target is the offering the converted parts are sent to.
	Target Offering
	// Path locates the part in the request.
	Path PartPath
	// Scope is the tenant or privacy scope of the conversion cache.
	Scope string
	// Vault is the privacy boundary's tokenizer (Egress.Vault), nil outside
	// one. The executor tokenizes a converter's text output with it before
	// the output enters the view.
	Vault PlaceholderVault
}

// ConversionEntry is a memoized conversion: the parts it produced and what
// produced them.
type ConversionEntry struct {
	Parts []Part
	Via   string
}

// ConversionCache memoizes conversions. Keys are built from the policy's
// scope, the converter's Name@Version and the part's digest (or URI), so a
// cache shared between tenants never serves one tenant's derivative to
// another as long as each sets its own scope. Implementations must be safe
// for concurrent use.
type ConversionCache interface {
	Get(ctx context.Context, key string) (ConversionEntry, bool)
	Put(ctx context.Context, key string, e ConversionEntry)
}

// ThinkingReplay says what happens to a reasoning part a target cannot
// verify, such as one signed by another vendor.
type ThinkingReplay string

const (
	// ThinkingDrop leaves the part out of the request. It is the default.
	ThinkingDrop ThinkingReplay = "drop"
	// ThinkingAsText sends the reasoning text as a plain text part instead.
	// A redacted part has no text and is dropped.
	ThinkingAsText ThinkingReplay = "text"
)

// ConversionPolicy is how the parts of a request are fitted to the offering
// that serves it. The zero value rejects every part the offering cannot
// take natively.
type ConversionPolicy struct {
	// Dial permits actions per modality. It is the lowest layer: the
	// modality dial of the catalog, the agent, the turn and the request
	// apply above it.
	Dial ModalityDial
	// Converters are the converters actions may use, in preference order.
	// A permitted action with no converter that accepts the part is a
	// rejection.
	Converters []Converter
	// Cache memoizes conversions. Nil uses one in-memory cache per policy
	// holder (the agent, or the conversion decorator).
	Cache ConversionCache
	// MaxCost caps the estimated cost of one request's conversions. A plan
	// estimated above it is rejected. Zero is no cap.
	MaxCost Cost
	// Scope is the tenant or privacy scope conversions are memoized under.
	Scope string
	// Thinking is what happens to a reasoning part the target cannot
	// verify. Empty is ThinkingDrop.
	Thinking ThinkingReplay
}

// IsZero reports whether p sets nothing.
func (p ConversionPolicy) IsZero() bool {
	return p.Dial.Default == "" && len(p.Dial.Per) == 0 && len(p.Converters) == 0 && p.Cache == nil &&
		p.MaxCost == 0 && p.Scope == "" && p.Thinking == ""
}

// Merge returns p with over applied: over's dial on top of p's, over's
// converters ahead of p's, and over's cache, cap, scope and thinking rule
// when set.
func (p ConversionPolicy) Merge(over ConversionPolicy) ConversionPolicy {
	out := p
	out.Dial = p.Dial.Merge(over.Dial)
	out.Converters = append(slices.Clone(over.Converters), p.Converters...)
	if over.Cache != nil {
		out.Cache = over.Cache
	}
	if over.MaxCost != 0 {
		out.MaxCost = over.MaxCost
	}
	if over.Scope != "" {
		out.Scope = over.Scope
	}
	if over.Thinking != "" {
		out.Thinking = over.Thinking
	}
	return out
}

// ConversionPlanner is implemented by a provider that fits parts to its
// offering before dispatch: the conversion decorator. A router calls it per
// member while it filters candidates, so a member whose plan rejects is
// removed from the request and a member whose plan converts is kept.
type ConversionPlanner interface {
	// PlanConversions plans req without doing it. The report lists every
	// decision; the estimate bounds the conversions' cost. A rejection is
	// an error matching ErrModalityUnsupported.
	PlanConversions(ctx context.Context, req Request) (ConversionReport, ConversionEstimate, error)
}

// OfferingReporter is implemented by an adapter that knows which offering
// it serves, such as a Google adapter on Vertex AI, whose endpoint reads
// gs:// URIs the Gemini API cannot.
type OfferingReporter interface {
	Offering() Offering
}
