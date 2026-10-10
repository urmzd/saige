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
// re-targeted at a model, a router profile or a preset. A target it cannot
// serve is an error wrapping ErrUnknownTarget, never a failure deferred to
// the next request.
type TargetSwitcher interface {
	Provider
	WithTarget(t Target) (Provider, error)
}

// ProviderWithTarget returns a variant of p serving t. The zero target
// returns p, and so does a model target naming p's configured model. A
// TargetSwitcher decides for itself; any other provider cannot be
// re-targeted, which is an error wrapping ErrUnknownTarget.
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
	return nil, fmt.Errorf("%w %s: provider %s cannot switch models", ErrUnknownTarget, t, NameOf(p))
}

// TargetModel is the WithTarget rule of a provider that serves one model at
// a time, such as an adapter: a model target names the model to switch to,
// and a profile or preset target is an error wrapping ErrUnknownTarget,
// since only a router defines them. provider names the caller in errors.
func TargetModel(t Target, provider string) (ModelID, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	if t.Model == "" {
		return "", fmt.Errorf("%w %s: provider %s has no profiles or presets", ErrUnknownTarget, t, provider)
	}
	return t.Model, nil
}

// RetargetMembers re-targets the members of a multi-provider decorator
// (named in errors) at a model target. A member that cannot be re-targeted
// is kept as-is, so a mixed chain still serves. A
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
		if _, ts := p.(TargetSwitcher); !ts {
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
