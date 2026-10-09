package ollama

import (
	"encoding/json"

	"github.com/urmzd/saige/agent/types"
)

// WithDials sets dials compiled on every request against the selected
// model, below any dials the request carries (see types.ResolveDials). A
// reasoning depth collapses to the think flag, recorded as mapped.
func WithDials(layers ...types.DialLayer) AdapterOption {
	return func(a *Adapter) {
		for _, l := range layers {
			a.dials = append(a.dials, l.Clone())
		}
	}
}

// WithDialPolicy sets how dials the model cannot honor are handled. A
// request's own policy replaces it.
func WithDialPolicy(p types.DialPolicy) AdapterOption {
	return func(a *Adapter) {
		p = p.Clone()
		a.dialPolicy = &p
	}
}

// compileDials returns a copy of the adapter, with its own client, that
// sends its configured dials and those of o compiled for one request. The
// copy has no dials left. Without dials it returns a unchanged.
func (a *Adapter) compileDials(o types.RequestOptions, tools []types.ToolDef, schema bool) (*Adapter, error) {
	all := a.EffectiveOptions().Merge(types.RequestOptions{Dials: o.Dials, DialLayers: o.DialLayers, DialPolicy: o.DialPolicy})
	if !all.HasDials() {
		return a, nil
	}
	eff, _, err := types.CompileOptions(a.Capabilities(), all, types.DialContext{Tools: len(tools) > 0, Schema: schema})
	if err != nil {
		return nil, err
	}
	client := *a.Client
	client.Think = eff.ReasoningEnabled
	options := map[string]any{}
	if a.Client.ChatOptions != nil {
		// Keep every configured option, num_ctx included, and overlay the
		// compiled sampling controls.
		data, err := json.Marshal(a.Client.ChatOptions)
		if err != nil {
			return nil, a.Capabilities().OptionError("options", "cannot encode options")
		}
		if err := json.Unmarshal(data, &options); err != nil {
			return nil, a.Capabilities().OptionError("options", "options must encode as an object")
		}
	}
	set := func(key string, v any, ok bool) {
		if ok {
			options[key] = v
		}
	}
	set("temperature", deref(eff.Temperature), eff.Temperature != nil)
	set("top_p", deref(eff.TopP), eff.TopP != nil)
	if eff.TopK != nil {
		set("top_k", int64(*eff.TopK), true)
	}
	set("seed", deref(eff.Seed), eff.Seed != nil)
	set("num_predict", deref(eff.MaxOutputTokens), eff.MaxOutputTokens != nil)
	if len(options) > 0 {
		client.ChatOptions = options
	}
	c := &Adapter{Client: &client, toolChoice: a.toolChoice}
	if eff.ToolChoice != nil {
		c.toolChoice = eff.ToolChoice
	}
	return c, nil
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
