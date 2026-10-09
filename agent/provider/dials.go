package provider

import (
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// withDials resolves cfg's dial layers and checks them against the model
// as a request without tools would compile them. A contractual dial the
// model cannot honor fails here rather than on the first request. When the
// cache dial is on and no prompt cache is configured, the model's prompt
// cache mode is selected.
func withDials(cfg Config, caps types.ModelCapabilities) (Config, error) {
	layers := cfg.DialLayers
	if layers == nil {
		if !cfg.Dials.IsZero() {
			layers = append(layers, types.DialLayer{Scope: types.DialScopeGlobal, Dials: cfg.Dials.Clone()})
		}
		if d := caps.DialMap.Defaults; !d.IsZero() {
			layers = append(layers, types.DialLayer{Scope: types.DialScopeModel, Dials: d.Clone()})
		}
	}
	cfg.DialLayers = layers
	if len(layers) == 0 {
		return cfg, nil
	}
	var pol types.DialPolicy
	if cfg.DialPolicy != nil {
		pol = *cfg.DialPolicy
	}
	if _, _, err := types.ResolveDials(caps, cfg.Options, types.DialContext{}, pol, layers...); err != nil {
		return cfg, err
	}
	if d := mergedDials(layers); d.Cache != nil && *d.Cache && cfg.PromptCache == nil {
		switch mode := caps.EffectiveDialMap().CacheMode; mode {
		case catalog.PromptCacheMarkers:
			cfg.PromptCache = &PromptCache{Mode: mode, TTL: "5m", Tools: true, System: true}
		case catalog.PromptCacheAutomatic:
			cfg.PromptCache = &PromptCache{Mode: mode}
		}
	}
	return cfg, nil
}

// mergedDials returns the dials the layers resolve to.
func mergedDials(layers []types.DialLayer) types.Dials {
	var d types.Dials
	for _, l := range layers {
		d = d.Merge(l.Dials)
	}
	return d
}
