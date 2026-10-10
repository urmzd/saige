package types

import (
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
}
