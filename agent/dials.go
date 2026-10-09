package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// WithDials sets the agent's model-neutral generation intents. They are sent
// with every call and compiled for the model that serves it, so a failover
// re-targets them instead of failing on a vendor parameter. Sub-agents
// inherit them unless their Options set WithDials.
func WithDials(d types.Dials) AgentOption {
	return func(c *AgentConfig) { c.Dials = d.Clone() }
}

// WithDialPolicy sets how dials the serving model cannot honor are handled.
// Use types.StrictDials to fail any call whose dials would be mapped or
// dropped.
func WithDialPolicy(p types.DialPolicy) AgentOption {
	return func(c *AgentConfig) {
		p = p.Clone()
		c.DialPolicy = &p
	}
}

// signedLoop tracks an open tool loop whose model turn carried signed
// reasoning. Such a loop must continue with the reasoning it started with:
// providers reject a thinking change in the middle of one.
type signedLoop struct {
	open bool
	// reasoning is the turn-level reasoning dial when the loop opened.
	reasoning *types.ReasoningDial
}

// observeAssistant opens the loop on a turn with tool calls and signed
// reasoning, and closes it on a turn without tool calls.
func (l *signedLoop) observeAssistant(m types.AssistantMessage, turn types.Dials) {
	calls, signed := false, false
	for _, c := range m.Content {
		switch v := c.(type) {
		case types.ToolUseContent:
			calls = true
		case types.ThinkingContent:
			signed = signed || v.Signature != ""
		}
	}
	switch {
	case !calls:
		*l = signedLoop{}
	case signed && !l.open:
		*l = signedLoop{open: true}
		if turn.Reasoning != nil {
			r := *turn.Reasoning
			l.reasoning = &r
		}
	}
}

// observeUser closes the loop on a user turn that is more than tool
// results.
func (l *signedLoop) observeUser(content []types.UserContent) {
	if slices.ContainsFunc(content, func(c types.UserContent) bool {
		_, result := c.(types.ToolResultContent)
		return !result
	}) {
		*l = signedLoop{}
	}
}

// dialLayers returns the dial layers of the next call: the active agent's
// own, then the conversation's. While a signed tool loop is open, a turn
// layer that changes reasoning carries the reasoning the loop started with
// as its Hold, so the change waits for the next user turn.
func dialLayers(ac activeContext, rc resolvedConfig) []types.DialLayer {
	var layers []types.DialLayer
	if !ac.dials.IsZero() {
		layers = append(layers, types.DialLayer{Scope: ac.dialScope, Dials: ac.dials.Clone()})
	}
	if rc.dials.IsZero() {
		return layers
	}
	turn := types.DialLayer{Scope: types.DialScopeTurn, Dials: rc.dials.Clone()}
	if rc.loop.open && rc.dials.Reasoning != nil {
		hold := rc.loop.reasoning
		if hold == nil {
			hold = ac.dials.Reasoning
		}
		if hold != nil && *hold != *rc.dials.Reasoning {
			h := *hold
			turn.Hold = &h
		}
	}
	return append(layers, turn)
}

// dialPolicy returns the policy for a call: one set on the context, as an
// eval run does, then the agent's.
func (a *Agent) dialPolicy(ctx context.Context) *types.DialPolicy {
	if p, ok := types.DialPolicyFromContext(ctx); ok {
		return &p
	}
	return a.cfg.DialPolicy
}

// attachDials adds the active dial layers to a call's options. Dials travel
// as request options, so a provider without them cannot receive any: a
// contractual dial is then an error, and an advisory one is logged and not
// sent. A tool-free call with a native response schema cannot carry options
// either; set the dial on the provider (provider.Config.Dials or a preset)
// to cover it.
func (a *Agent) attachDials(ctx context.Context, ac activeContext, opts *types.RequestOptions, tools []types.ToolDef) (*types.RequestOptions, error) {
	pol := a.dialPolicy(ctx)
	// A policy alone matters only to a provider configured with dials.
	if len(ac.dialLayers) == 0 && (pol == nil || !configuredDials(ac.provider)) {
		return opts, nil
	}
	if a.output(ctx).native() && len(tools) == 0 {
		if len(ac.dialLayers) > 0 {
			a.cfg.Logger.Warn("dials are not sent with a native response schema; set them on the provider",
				"agent", a.cfg.Name, "provider", types.ProviderName(ac.provider))
		}
		return opts, nil
	}
	if _, ok := ac.provider.(types.OptionsProvider); !ok {
		merged := mergeLayers(ac.dialLayers)
		for _, n := range merged.Names() {
			if n.Class(merged) == types.DialContractual {
				return nil, fmt.Errorf("%w: dial %s: provider %q does not accept request options",
					types.ErrInvalidModelConfig, n, types.ProviderName(ac.provider))
			}
		}
		if len(ac.dialLayers) > 0 {
			a.cfg.Logger.Warn("dials are not sent: the provider does not accept request options",
				"agent", a.cfg.Name, "provider", types.ProviderName(ac.provider))
		}
		return opts, nil
	}
	out := types.RequestOptions{}
	if opts != nil {
		out = opts.Clone()
	}
	out.DialLayers = append(out.DialLayers, ac.dialLayers...)
	if pol != nil {
		p := pol.Clone()
		out.DialPolicy = &p
	}
	return &out, nil
}

// configuredDials reports whether any provider in p's chain was built with
// dials, such as a preset entry behind a router.
func configuredDials(p types.Provider) bool {
	found := false
	wrapper.Walk(p, func(q types.Provider) bool {
		if o, ok := types.ProviderEffectiveOptions(q); ok && o.HasDials() {
			found = true
		}
		return !found
	})
	return found
}

func mergeLayers(layers []types.DialLayer) types.Dials {
	var d types.Dials
	for _, l := range layers {
		d = d.Merge(l.Dials)
	}
	return d
}

// localDialRoute describes how a call's dials compile when the provider
// reports no routes of its own, as a single adapter does: the adapter
// compiles the same dials the same way, so the route names what it sends.
// It is nil for a call without dials, and for a router or another
// multi-provider decorator, which reports each attempt itself.
func localDialRoute(provider types.Provider, opts *types.RequestOptions, tools []types.ToolDef) *types.RouteDelta {
	if _, multi := wrapper.As[wrapper.MultiWrapper](provider); multi {
		return nil
	}
	caps, ok := types.ProviderCapabilities(provider)
	if !ok {
		return nil
	}
	all, _ := types.ProviderEffectiveOptions(provider)
	if opts != nil {
		all = all.Merge(*opts)
	}
	if !all.HasDials() {
		return nil
	}
	ctx := types.DialContext{Tools: len(tools) > 0}
	if r, ok := wrapper.As[types.DialSurfaceReporter](provider); ok {
		ctx.Surface = r.DialSurface()
	}
	eff, rep, err := types.CompileOptions(caps, all, ctx)
	if rep == nil {
		return nil
	}
	d := &types.RouteDelta{Provider: wrapper.InnermostName(provider), Model: types.ProviderModel(provider), Dials: rep}
	if err == nil {
		d.Options = &eff
	}
	return d
}
