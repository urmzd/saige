package convert

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Provider is the conversion decorator. Before each call it plans the
// request's parts against the offering of the provider it wraps, rejects a
// request whose plan rejects, runs the planned conversions on a copy of the
// messages, sends a types.ConversionDelta with the executed report ahead of
// the inner provider's output, and calls the inner provider with the
// converted copy. The caller's messages are never modified.
//
// A request whose parts are all native goes to the inner provider
// unchanged and emits no ConversionDelta. A provider that reports no
// capabilities has no offering to plan against, and its requests pass
// through.
//
// The modality dial is spent here: it is removed from the request options
// before they reach the inner provider.
type Provider struct {
	inner  types.Provider
	policy types.ConversionPolicy
	layers []types.DialLayer
	cache  types.ConversionCache
}

var (
	_ types.Provider                 = (*Provider)(nil)
	_ types.NamedProvider            = (*Provider)(nil)
	_ types.ModelProvider            = (*Provider)(nil)
	_ types.ModelSwitcher            = (*Provider)(nil)
	_ types.TargetSwitcher           = (*Provider)(nil)
	_ types.CapabilityReporter       = (*Provider)(nil)
	_ types.StructuredOutputProvider = (*Provider)(nil)
	_ types.OptionsProvider          = (*Provider)(nil)
	_ types.OptionsReporter          = (*Provider)(nil)
	_ types.SessionProvider          = (*Provider)(nil)
	_ types.Closer                   = (*Provider)(nil)
	_ types.ConversionPlanner        = (*Provider)(nil)
	_ wrapper.Wrapper                = (*Provider)(nil)
)

// New wraps inner with policy. layers are the modality dial layers inner
// was built with, such as a catalog preset entry's; only their modality
// dials are read. When the policy has no cache, the decorator keeps its own
// in-memory one.
func New(inner types.Provider, policy types.ConversionPolicy, layers ...types.DialLayer) *Provider {
	p := &Provider{inner: inner, policy: policy, cache: policy.Cache}
	for _, l := range layers {
		if l.Dials.Modality != nil {
			p.layers = append(p.layers, types.DialLayer{Scope: l.Scope, Dials: types.Dials{Modality: l.Dials.Modality}}.Clone())
		}
	}
	if p.cache == nil {
		p.cache = NewMemoryCache(0)
	}
	return p
}

// derive returns a decorator with the same policy and cache around inner.
func (p *Provider) derive(inner types.Provider) *Provider {
	return &Provider{inner: inner, policy: p.policy, layers: p.layers, cache: p.cache}
}

// Name implements types.NamedProvider with the inner provider's name: the
// decorator does not change which vendor serves.
func (p *Provider) Name() string { return types.NameOf(p.inner) }

// Model implements types.ModelProvider.
func (p *Provider) Model() string { return types.ProviderModel(p.inner) }

// WithModel implements types.ModelSwitcher, keeping the policy.
func (p *Provider) WithModel(model string) types.Provider {
	inner := types.ProviderWithModel(p.inner, model)
	if inner == p.inner {
		return p
	}
	return p.derive(inner)
}

// WithTarget implements types.TargetSwitcher, keeping the policy.
func (p *Provider) WithTarget(t types.Target) (types.Provider, error) {
	inner, err := types.ProviderWithTarget(p.inner, t)
	if err != nil {
		return nil, err
	}
	if inner == p.inner {
		return p, nil
	}
	return p.derive(inner), nil
}

// Capabilities implements types.CapabilityReporter by forwarding.
// Capabilities that need request options are dropped when the inner
// provider cannot receive them, as every decorator does.
func (p *Provider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(p.inner)
	if !types.AcceptsOptions(p.inner) {
		caps = caps.Without(types.CapToolChoice, types.CapParallelToolControl)
	}
	return caps
}

// SupportsSchema implements types.StructuredOutputProvider by forwarding.
func (p *Provider) SupportsSchema() bool { return types.AcceptsSchema(p.inner) }

// SupportsOptions implements types.OptionsProvider by forwarding.
func (p *Provider) SupportsOptions() bool { return types.AcceptsOptions(p.inner) }

// EffectiveOptions implements types.OptionsReporter by forwarding.
func (p *Provider) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(p.inner)
	return o
}

// NewSession implements types.SessionProvider, sharing the cache.
func (p *Provider) NewSession() types.Provider {
	inner := types.NewProviderSession(p.inner)
	if inner == p.inner {
		return p
	}
	return p.derive(inner)
}

// Unwrap returns the inner provider. See package wrapper.
func (p *Provider) Unwrap() types.Provider { return p.inner }

// Close implements types.Closer by closing the inner provider.
func (p *Provider) Close() error { return types.CloseProvider(p.inner) }

// Target returns the offering the inner provider serves: the one an adapter
// reports itself (types.OfferingReporter), else the one its capabilities
// carry, else one projected from its capabilities. The bool is false when
// the provider reports no capabilities.
func Target(inner types.Provider) (types.Offering, bool) {
	if r, ok := wrapper.Innermost(inner).(types.OfferingReporter); ok {
		return r.Offering(), true
	}
	caps, ok := types.ProviderCapabilities(inner)
	if !ok {
		return types.Offering{}, false
	}
	if caps.Offering != nil {
		return caps.Offering.Clone(), true
	}
	return types.OfferingFromCapabilities(caps), true
}

// policyFor merges the caller's runtime policy over the decorator's. The
// runtime policy's dial is not merged here: it is a layer (see layersFor).
func (p *Provider) policyFor(rt Runtime) types.ConversionPolicy {
	over := rt.Policy
	over.Dial = types.ModalityDial{}
	pol := p.policy.Merge(over)
	if pol.Cache == nil {
		pol.Cache = p.cache
	}
	return pol
}

// layersFor lists the modality dial layers of a request, lowest first: the
// decorator's own (the scopes the provider was built from), the runtime
// policy's dial at agent scope, the runtime's layers, and the request's.
func (p *Provider) layersFor(rt Runtime, req types.Request) []types.DialLayer {
	layers := append([]types.DialLayer(nil), p.layers...)
	if d := rt.Policy.Dial; d.Default != "" || len(d.Per) > 0 {
		c := d.Clone()
		layers = append(layers, types.DialLayer{Scope: types.DialScopeAgent, Dials: types.Dials{Modality: &c}})
	}
	layers = append(layers, rt.Layers...)
	if req.Options != nil {
		layers = append(layers, req.Options.ModalityLayers()...)
	}
	return layers
}

// plan plans req for the inner provider's offering. ok is false when there
// is nothing to plan against.
func (p *Provider) plan(ctx context.Context, req types.Request) (Plan, Runtime, bool, error) {
	if nested(ctx) {
		return Plan{}, Runtime{}, false, nil
	}
	target, ok := Target(p.inner)
	if !ok {
		return Plan{}, Runtime{}, false, nil
	}
	rt, _ := RuntimeFrom(ctx)
	pl, err := PlanConversions(target, req.Messages, p.policyFor(rt), p.layersFor(rt, req)...)
	return pl, rt, true, err
}

// PlanConversions implements types.ConversionPlanner.
func (p *Provider) PlanConversions(ctx context.Context, req types.Request) (types.ConversionReport, types.ConversionEstimate, error) {
	pl, _, ok, err := p.plan(ctx, req)
	if !ok {
		return types.ConversionReport{}, types.ConversionEstimate{}, nil
	}
	return pl.Report(), pl.Estimate, err
}

// Stream implements types.Provider. Options or a schema the inner provider
// cannot receive are rejected with types.ErrInvalidModelConfig, as every
// decorator does; options that carried only the modality dial are dropped
// once it is spent.
func (p *Provider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	out := req
	if req.Options != nil && len(req.Options.ModalityLayers()) > 0 {
		o := req.Options.WithoutModality()
		out.Options = &o
		if emptyOptions(o) {
			out.Options = nil
		}
	}
	if out.Options != nil && !types.AcceptsOptions(p.inner) {
		return nil, p.unsupported(types.ErrOptionsUnsupported)
	}
	if out.Schema != nil && !types.AcceptsSchema(p.inner) {
		return nil, p.unsupported(types.ErrSchemaUnsupported)
	}
	pl, rt, ok, err := p.plan(ctx, req)
	if err != nil {
		return nil, err
	}
	if !ok || !pl.Converts() {
		return p.inner.Stream(ctx, out)
	}
	msgs, rep, err := pl.Apply(ctx, req.Messages, rt, p.policyFor(rt).Cache)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	src, err := p.inner.Stream(ctx, out)
	if err != nil {
		return nil, err
	}
	ch := make(chan types.Delta)
	go func() {
		defer close(ch)
		select {
		case ch <- types.ConversionDelta{Report: rep}:
		case <-ctx.Done():
			go drain(src)
			return
		}
		for d := range src {
			select {
			case ch <- d:
			case <-ctx.Done():
				go drain(src)
				return
			}
		}
	}()
	return ch, nil
}

func (p *Provider) unsupported(err error) error {
	return &types.ProviderError{Provider: types.NameOf(p.inner), Model: types.ProviderModel(p.inner),
		Kind: types.ErrorKindPermanent, Err: fmt.Errorf("%w: provider %q", err, types.NameOf(p.inner))}
}

// emptyOptions reports whether o asks for nothing.
func emptyOptions(o types.RequestOptions) bool {
	return len(o.OptionNames()) == 0 && !o.HasDials() && o.DialPolicy == nil
}

func drain(ch <-chan types.Delta) {
	for range ch {
	}
}
