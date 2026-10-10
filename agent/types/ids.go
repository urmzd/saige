package types

import (
	"errors"
	"fmt"
)

// ProviderName names a provider adapter or model vendor: "anthropic",
// "openai", "google", "ollama". The catalog lists the names it knows.
type ProviderName string

// ModelID is a vendor model identifier, such as "claude-haiku-5-5" or
// "gpt-6-luna". It may carry a dated or tagged suffix the catalog matches
// by prefix.
type ModelID string

// ProfileID names one complete provider configuration a router can serve,
// such as "default/anthropic/claude-haiku-5-5".
type ProfileID string

// PresetName names a catalog preset, and the router group built from its
// chain.
type PresetName string

// ErrInvalidTarget reports a Target that sets no field or more than one.
var ErrInvalidTarget = errors.New("invalid target")

// ErrUnknownTarget reports a Target a provider cannot serve: a profile or
// preset it does not define, or a model it cannot switch to.
var ErrUnknownTarget = errors.New("unknown target")

// Target is what a ConfigPart pins: one model, one router profile, or one
// preset (a router group). Exactly one field is set; the zero Target means
// no change.
type Target struct {
	Model   ModelID    `json:"model,omitempty"`
	Profile ProfileID  `json:"profile,omitempty"`
	Preset  PresetName `json:"preset,omitempty"`
}

// ModelTarget pins a model.
func ModelTarget(m ModelID) Target { return Target{Model: m} }

// ProfileTarget pins a router profile.
func ProfileTarget(p ProfileID) Target { return Target{Profile: p} }

// PresetTarget pins a preset's chain.
func PresetTarget(p PresetName) Target { return Target{Preset: p} }

// IsZero reports whether the target pins nothing.
func (t Target) IsZero() bool { return t == Target{} }

// Validate reports ErrInvalidTarget unless exactly one field is set.
func (t Target) Validate() error {
	n := 0
	for _, set := range []bool{t.Model != "", t.Profile != "", t.Preset != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%w: set exactly one of model, profile or preset", ErrInvalidTarget)
	}
	return nil
}

// String renders the target as "model:<id>", "profile:<id>" or
// "preset:<name>", and the zero target as "".
func (t Target) String() string {
	switch {
	case t.Model != "":
		return "model:" + string(t.Model)
	case t.Profile != "":
		return "profile:" + string(t.Profile)
	case t.Preset != "":
		return "preset:" + string(t.Preset)
	}
	return ""
}

// TargetSwitcher is an optional interface for providers that can be
// re-targeted at a model, a router profile or a preset. Unlike
// ModelSwitcher it reports a target it cannot serve as an error, wrapping
// ErrUnknownTarget, instead of deferring the failure to the next request.
type TargetSwitcher interface {
	Provider
	WithTarget(t Target) (Provider, error)
}

// ProviderWithTarget returns a variant of p serving t. The zero target
// returns p. A TargetSwitcher decides for itself; otherwise a model target
// goes through ModelSwitcher, and a profile or preset target is an error,
// since only a router defines them.
func ProviderWithTarget(p Provider, t Target) (Provider, error) {
	if t.IsZero() {
		return p, nil
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if ts, ok := p.(TargetSwitcher); ok {
		return ts.WithTarget(t)
	}
	if t.Model == "" {
		return nil, fmt.Errorf("%w %s: provider %s has no profiles or presets", ErrUnknownTarget, t, NameOf(p))
	}
	if ProviderModel(p) == string(t.Model) {
		return p, nil
	}
	if ms, ok := p.(ModelSwitcher); ok {
		return ms.WithModel(string(t.Model)), nil
	}
	return nil, fmt.Errorf("%w %s: provider %s cannot switch models", ErrUnknownTarget, t, NameOf(p))
}

// RetargetMembers re-targets the members of a multi-provider decorator
// (named in errors) at a model target. A member that can switch neither
// targets nor models is kept as-is, so a mixed chain still serves. A
// profile or preset target is an error: only a router defines them.
func RetargetMembers(members []Provider, t Target, decorator string) ([]Provider, error) {
	if err := t.Validate(); err != nil && !t.IsZero() {
		return nil, err
	}
	if t.Model == "" && !t.IsZero() {
		return nil, fmt.Errorf("%w %s: a %s chain has no profiles or presets", ErrUnknownTarget, t, decorator)
	}
	out := make([]Provider, len(members))
	for i, p := range members {
		_, ts := p.(TargetSwitcher)
		_, ms := p.(ModelSwitcher)
		if !ts && !ms {
			out[i] = p
			continue
		}
		q, err := ProviderWithTarget(p, t)
		if err != nil {
			return nil, err
		}
		out[i] = q
	}
	return out, nil
}
