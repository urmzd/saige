package agent

import (
	"context"
	"slices"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// conversionPolicy folds the extractors into the policy: one extract
// converter per media type, and extract permitted for each of their
// modalities the policy's dial does not already name. The policy gets an
// in-memory cache when it has none, which sub-agents share through the
// inherited config.
func conversionPolicy(p types.ConversionPolicy, extractors map[types.MediaType]types.Extractor) types.ConversionPolicy {
	if len(extractors) > 0 {
		mts := make([]types.MediaType, 0, len(extractors))
		for mt := range extractors {
			mts = append(mts, mt)
		}
		slices.Sort(mts)
		per := map[types.Modality][]types.ModalityAction{}
		for _, mt := range mts {
			p.Converters = append(p.Converters, convert.Extract(extractors[mt], mt))
			m := mt.Modality()
			if _, named := p.Dial.Per[m]; !named {
				per[m] = []types.ModalityAction{types.ActExtract}
			}
		}
		p.Dial = p.Dial.Merge(types.ModalityDial{Per: per})
	}
	if p.Cache == nil {
		p.Cache = convert.NewMemoryCache(0)
	}
	return p
}

// splitModality separates the modality dial from the other dials of each
// layer. The other dials travel as request options; the modality dial goes
// to the conversion decorators through the runtime, so it applies even to a
// call that carries no options.
func splitModality(layers []types.DialLayer) (rest, modality []types.DialLayer) {
	for _, l := range layers {
		if l.Dials.Modality != nil {
			modality = append(modality, types.DialLayer{Scope: l.Scope, Dials: types.Dials{Modality: l.Dials.Modality}}.Clone())
			l = l.Clone()
			l.Dials.Modality = nil
			if l.Dials.IsZero() && l.Hold == nil {
				continue
			}
		}
		rest = append(rest, l)
	}
	return rest, modality
}

// withConversion returns ctx carrying the conversion runtime of the next
// model call: the agent's policy (its dial at agent scope), the modality
// layers of the active agent and the turn, and the durable runner.
func (a *Agent) withConversion(ctx context.Context, modality []types.DialLayer) context.Context {
	return convert.WithRuntime(ctx, convert.Runtime{Policy: a.cfg.Conversion, Layers: modality, Steps: a.cfg.StepRunner})
}

// converting returns the provider a model call goes to: p itself when its
// chain plans conversions (a provider built by provider.Build, or a router
// or fallback chain of them), or p behind a conversion decorator.
func (a *Agent) converting(p types.Provider) types.Provider {
	if _, ok := wrapper.As[types.ConversionPlanner](p); ok {
		return p
	}
	cp, err := convert.New(p, convert.Config{Policy: types.ConversionPolicy{Cache: a.cfg.Conversion.Cache}})
	if err != nil {
		return p // no provider: the call reports that itself
	}
	return cp
}

// conversionEstimate bounds what the call's planned conversions cost, for
// the budget reservation: the largest estimate any planner in the chain
// makes, since one attempt serves the call. A plan that rejects estimates
// nothing; the call then fails before any conversion runs.
func (a *Agent) conversionEstimate(ctx context.Context, p types.Provider, req types.Request) types.ConversionEstimate {
	var est types.ConversionEstimate
	if !estimateUnder(ctx, p, req, &est, 0) {
		if _, e, err := a.converting(p).(types.ConversionPlanner).PlanConversions(ctx, req); err == nil {
			est = e
		}
	}
	return est
}

// estimateUnder raises est to each planner's estimate under p, without
// descending below a planner, which plans for everything it wraps. It
// reports whether it found one.
func estimateUnder(ctx context.Context, p types.Provider, req types.Request, est *types.ConversionEstimate, depth int) bool {
	if p == nil || depth > 64 {
		return false
	}
	if pl, ok := p.(types.ConversionPlanner); ok {
		if _, e, err := pl.PlanConversions(ctx, req); err == nil {
			est.InputTokens = max(est.InputTokens, e.InputTokens)
			est.OutputTokens = max(est.OutputTokens, e.OutputTokens)
			est.Cost = max(est.Cost, e.Cost)
		}
		return true
	}
	found := false
	for _, m := range wrapper.Members(p) {
		found = estimateUnder(ctx, m, req, est, depth+1) || found
	}
	return found
}

// servingProvider names the provider that produced a turn: the route's, or
// the adapter beneath p's decorators.
func servingProvider(p types.Provider, route *types.RouteDelta) string {
	if route != nil && route.Provider != "" {
		return route.Provider
	}
	return wrapper.InnermostName(p)
}

// stampThinkingOrigin records which provider signed each reasoning part of
// a turn, so a later attempt on another provider leaves it out of the view
// instead of replaying a signature that provider cannot verify.
func stampThinkingOrigin(parts []types.AssistantPart, origin string) {
	if origin == "" {
		return
	}
	for i, p := range parts {
		if t, ok := p.(types.ThinkingPart); ok && t.Origin == "" {
			t.Origin = origin
			parts[i] = t
		}
	}
}

// routePart records the turn's route and, when the serving attempt
// converted media, its executed conversion report.
func routePart(p types.Provider, route *types.RouteDelta, conversions *types.ConversionReport) types.RoutePart {
	var rp types.RoutePart
	if route != nil {
		rp = types.RoutePartFrom(*route)
	} else {
		rp = types.RoutePart{Provider: wrapper.InnermostName(p), Model: types.ProviderModel(p)}
	}
	if conversions != nil {
		c := conversions.Clone()
		rp.Conversions = &c
	}
	return rp
}
