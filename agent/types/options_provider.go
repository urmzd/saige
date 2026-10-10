package types

import "time"

// OptionsProvider is an optional interface for providers whose Stream
// applies Request.Options on top of their configured defaults. The agent
// loop uses it to send a tool choice. A provider without it, or whose
// SupportsOptions reports false, cannot receive a required or named tool
// choice, and the loop rejects such a request rather than drop the control
// (see ModelCapabilities.ValidateOptions).
//
// Decorators that wrap a provider implement it and reject options their
// inner provider cannot receive when the request arrives.
type OptionsProvider interface {
	Provider
	SupportsOptions() bool
}

// AcceptsOptions reports whether p applies Request.Options.
func AcceptsOptions(p Provider) bool {
	op, ok := p.(OptionsProvider)
	return ok && op.SupportsOptions()
}

// Forced reports whether the choice obliges the model to call a tool. A forced
// choice is applied to one turn and then reverts to auto.
func (c ToolChoice) Forced() bool {
	return c.Mode == ToolChoiceRequired || c.Mode == ToolChoiceNamed
}

// OptionsReporter is an optional interface for providers that can state the
// options they send when a call adds none of its own: the configured controls,
// as the adapter will put them on the wire. A router merges a request's
// overrides onto them to check eligibility against what will actually be
// sent, and records them on each RouteDelta. Decorators forward it.
type OptionsReporter interface {
	EffectiveOptions() RequestOptions
}

// ProviderEffectiveOptions returns p's configured options. The bool is false
// when p does not report them.
func ProviderEffectiveOptions(p Provider) (RequestOptions, bool) {
	if r, ok := p.(OptionsReporter); ok {
		return r.EffectiveOptions().Clone(), true
	}
	return RequestOptions{}, false
}

// PresetDefaults are the agent-level settings a preset carries next to its
// provider chain. Empty fields leave the agent's own defaults.
type PresetDefaults struct {
	// Name is the preset name.
	Name string
	// CatalogRevision identifies the catalog the preset was resolved from.
	CatalogRevision string
	// ToolChoice is the agent-level default tool choice. A forced choice
	// applies to one turn.
	ToolChoice *ToolChoice
	// OutputMode is "", "native", "tool" or "prompt".
	OutputMode string
	// LLMTimeout bounds one provider call. Zero leaves the agent default.
	LLMTimeout time.Duration
	// Compaction is the preset's compaction strategy. Nil leaves the
	// agent's.
	Compaction *CompactConfig
}

// Preset is a provider chain built from a declared configuration, together
// with the agent-level defaults declared next to it.
type Preset interface {
	Provider() Provider
	Defaults() PresetDefaults
}
