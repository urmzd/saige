package convert

import (
	"context"
	"errors"
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
	wrapper.Base
	policy types.ConversionPolicy
	layers []types.DialLayer
	cache  types.ConversionCache
}

var (
	_ types.Provider                 = (*Provider)(nil)
	_ types.NamedProvider            = (*Provider)(nil)
	_ types.ModelProvider            = (*Provider)(nil)
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

// Config is a conversion decorator's policy and dial layers.
type Config struct {
	// Policy decides what happens to each part the inner provider's
	// offering cannot take. When it has no cache, the decorator keeps its
	// own in-memory one.
	Policy types.ConversionPolicy
	// Layers are the modality dial layers the inner provider was built
	// with, such as a catalog preset entry's; only their modality dials
	// are read.
	Layers []types.DialLayer
}

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithLayers appends dial layers to Config.Layers.
func WithLayers(layers ...types.DialLayer) Option {
	return func(c *Config) { c.Layers = append(c.Layers, layers...) }
}

// New wraps inner with cfg.Policy. A nil inner is an error wrapping
// types.ErrInvalidConfig.
func New(inner types.Provider, cfg Config, opts ...Option) (*Provider, error) {
	for _, o := range opts {
		o(&cfg)
	}
	if inner == nil {
		return nil, fmt.Errorf("%w: convert: no provider to wrap", types.ErrInvalidConfig)
	}
	return newProvider(inner, cfg), nil
}

func newProvider(inner types.Provider, cfg Config) *Provider {
	p := &Provider{policy: cfg.Policy, cache: cfg.Policy.Cache}
	for _, l := range cfg.Layers {
		if l.Dials.Modality != nil {
			p.layers = append(p.layers, types.DialLayer{Scope: l.Scope, Dials: types.Dials{Modality: l.Dials.Modality}}.Clone())
		}
	}
	if p.cache == nil {
		p.cache = NewMemoryCache(0)
	}
	p.Base = wrapper.NewBase(inner, p.rewrap)
	return p
}

// rewrap returns a decorator with the same policy and cache around inner.
func (p *Provider) rewrap(inner types.Provider) types.Provider {
	c := *p
	c.Base = wrapper.NewBase(inner, c.rewrap)
	return &c
}

// SupportsSchema implements types.StructuredOutputProvider by forwarding:
// the decorator does not change whether a schema can be enforced.
func (p *Provider) SupportsSchema() bool { return types.AcceptsSchema(p.Inner) }

// SupportsOptions implements types.OptionsProvider by forwarding.
func (p *Provider) SupportsOptions() bool { return types.AcceptsOptions(p.Inner) }

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
	if nested(ctx) || p.Inner == nil {
		return Plan{}, Runtime{}, false, nil
	}
	target, ok := Target(p.Inner)
	if !ok {
		return Plan{}, Runtime{}, false, nil
	}
	rt, _ := RuntimeFrom(ctx)
	pl, err := PlanConversions(target, req.Messages, p.policyFor(rt), p.layersFor(rt, req)...)
	if eg, ok := types.EgressFrom(ctx); ok && eg.RequireText {
		err = requireText(&pl, eg, err)
	}
	return pl, rt, true, err
}

// requireText rejects every part a plan would send as opaque media, natively,
// lowered or converted to other media, under a privacy boundary that lets
// media leave only as text. A router then removes the member, and a member
// that would transcribe, describe, extract or omit the part stays.
func requireText(pl *Plan, eg types.Egress, err error) error {
	var rej *RejectError
	if !errors.As(err, &rej) {
		rej = &RejectError{Offering: pl.Offering}
	}
	added := false
	for i := range pl.Decisions {
		d := &pl.Decisions[i]
		switch d.Action {
		case types.DecisionNative, types.DecisionLowered, types.DecisionConverted:
		default:
			continue
		}
		if !eg.IsOpaque(d.part) {
			continue
		}
		d.Reason = joinReason("the privacy policy sends media only as text", d.Reason)
		d.Action, d.Via = types.DecisionRejected, ""
		rej.Rejected = append(rej.Rejected, d.ConversionDecision)
		added = true
	}
	if !added {
		return err
	}
	return rej
}

func joinReason(a, b string) string {
	if b == "" {
		return a
	}
	return a + "; " + b
}

// checkView refuses a view that still carries opaque media under a
// boundary that requires text: media in an assistant turn, which is never
// planned, or a part a failed conversion fell back to media for.
func checkView(ctx context.Context, msgs []types.Message, offering string) error {
	eg, ok := types.EgressFrom(ctx)
	if !ok || !eg.RequireText {
		return nil
	}
	rej := &RejectError{Offering: offering}
	for mi, m := range msgs {
		for pi, part := range types.PartsOf(m) {
			check := func(p types.Part, nested int) {
				if !eg.IsOpaque(p) {
					return
				}
				src, _ := types.SourceOf(p)
				rej.Rejected = append(rej.Rejected, types.ConversionDecision{
					Path: types.PartPath{Message: mi, Part: pi, Nested: nested}, Kind: p.Kind(), MediaType: src.MediaType,
					Digest: src.Digest, Action: types.DecisionRejected, Reason: "the privacy policy sends media only as text"})
			}
			check(part, -1)
			if tr, ok := part.(types.ToolResultPart); ok {
				for ni, np := range tr.Parts {
					check(np, ni)
				}
			}
		}
	}
	if len(rej.Rejected) > 0 {
		return rej
	}
	return nil
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
	if out.Options != nil && !types.AcceptsOptions(p.Inner) {
		return nil, p.unsupported(types.ErrOptionsUnsupported)
	}
	if out.Schema != nil && !types.AcceptsSchema(p.Inner) {
		return nil, p.unsupported(types.ErrSchemaUnsupported)
	}
	pl, rt, ok, err := p.plan(ctx, req)
	if err != nil {
		return nil, err
	}
	if !ok || !pl.Converts() {
		if err := checkView(ctx, out.Messages, pl.Offering); err != nil {
			return nil, err
		}
		return p.Inner.Stream(ctx, out)
	}
	msgs, rep, err := pl.Apply(ctx, req.Messages, rt, p.policyFor(rt).Cache)
	if err != nil {
		return nil, err
	}
	if err := checkView(ctx, msgs, pl.Offering); err != nil {
		return nil, err
	}
	out.Messages = msgs
	src, err := p.Inner.Stream(ctx, out)
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
	return &types.ProviderError{Provider: types.NameOf(p.Inner), Model: types.ProviderModel(p.Inner),
		Kind: types.ErrorKindPermanent, Err: fmt.Errorf("%w: provider %q", err, types.NameOf(p.Inner))}
}

// emptyOptions reports whether o asks for nothing.
func emptyOptions(o types.RequestOptions) bool {
	return len(o.OptionNames()) == 0 && !o.HasDials() && o.DialPolicy == nil
}

func drain(ch <-chan types.Delta) {
	for range ch {
	}
}
