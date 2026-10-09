package google

import "github.com/urmzd/saige/agent/types"

// WithDials sets dials compiled on every request against the selected
// model, below any dials the request carries (see types.ResolveDials).
func WithDials(layers ...types.DialLayer) Option {
	return func(a *Adapter) {
		for _, l := range layers {
			a.dials = append(a.dials, l.Clone())
		}
	}
}

// WithDialPolicy sets how dials the model cannot honor are handled. A
// request's own policy replaces it.
func WithDialPolicy(p types.DialPolicy) Option {
	return func(a *Adapter) {
		p = p.Clone()
		a.dialPolicy = &p
	}
}

// compileDials returns a copy of the adapter with its configured dials and
// those of o compiled for one request and applied as options. The copy has
// no dials left. Without dials it returns a unchanged.
func (a *Adapter) compileDials(o types.RequestOptions, tools []types.ToolDef, schema bool) (*Adapter, error) {
	all := a.EffectiveOptions().Merge(types.RequestOptions{Dials: o.Dials, DialLayers: o.DialLayers, DialPolicy: o.DialPolicy})
	if !all.HasDials() {
		return a, nil
	}
	eff, _, err := types.CompileOptions(a.Capabilities(), all, types.DialContext{Tools: len(tools) > 0, Schema: schema})
	if err != nil {
		return nil, err
	}
	c, err := a.withRequestOptions(eff)
	if err != nil {
		return nil, err
	}
	c.dials, c.dialPolicy = nil, nil
	return c, nil
}
