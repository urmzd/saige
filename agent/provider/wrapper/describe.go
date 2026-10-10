package wrapper

import "github.com/urmzd/saige/agent/types"

// ProviderInfo is what a provider stack reports about itself, gathered in
// one place from its optional interfaces.
type ProviderInfo struct {
	// Name is the outermost provider's name, such as "retry(openai)".
	Name string
	// Vendor is the name of the provider at the bottom of the decorator
	// chain, such as "openai" (see InnermostName).
	Vendor string
	// Model is the configured model, empty when no provider reports one.
	Model string
	// Capabilities are the reported capabilities; CapabilitiesKnown is
	// false when no provider in the chain reports any.
	Capabilities      types.ModelCapabilities
	CapabilitiesKnown bool
	// Options are the options the provider sends; OptionsKnown is false
	// when no provider in the chain reports them.
	Options      types.RequestOptions
	OptionsKnown bool
	// AcceptsSchema and AcceptsOptions report whether a request may carry
	// a response schema or per-request options.
	AcceptsSchema  bool
	AcceptsOptions bool
	// Retargetable reports whether the provider can be re-targeted
	// (types.TargetSwitcher).
	Retargetable bool
	// Offering is the offering a member reports, when one does.
	Offering *types.Offering
	// DialSurface is the vendor API surface a member reports, such as
	// types.SurfaceChat, empty when none does.
	DialSurface string
}

// Describe reports what p says about itself. Each field comes from p when
// p implements the interface, and otherwise from the first provider in its
// chain that does (see As), so a decorator that forwards nothing still
// describes the provider it wraps.
func Describe(p types.Provider) ProviderInfo {
	info := ProviderInfo{
		Name:           types.NameOf(p),
		Vendor:         InnermostName(p),
		AcceptsSchema:  types.AcceptsSchema(p),
		AcceptsOptions: types.AcceptsOptions(p),
	}
	if m, ok := As[types.ModelProvider](p); ok {
		info.Model = m.Model()
	}
	if c, ok := As[types.CapabilityReporter](p); ok {
		info.Capabilities, info.CapabilitiesKnown = c.Capabilities(), true
	}
	if o, ok := As[types.OptionsReporter](p); ok {
		info.Options, info.OptionsKnown = o.EffectiveOptions().Clone(), true
	}
	_, info.Retargetable = p.(types.TargetSwitcher)
	if o, ok := As[types.OfferingReporter](p); ok {
		off := o.Offering()
		info.Offering = &off
	}
	if d, ok := As[types.DialSurfaceReporter](p); ok {
		info.DialSurface = d.DialSurface()
	}
	return info
}
