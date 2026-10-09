package openai

import "github.com/urmzd/saige/agent/types"

var (
	_ types.DialSurfaceReporter = (*Adapter)(nil)
	_ types.DialSurfaceReporter = (*ResponsesAdapter)(nil)
)

// WithDials sets dials compiled on every request against the selected
// model, below any dials the request carries (see types.ResolveDials). A
// dial compiles for the API in use: with tools on Chat Completions, a model
// that takes tools there only without reasoning gets effort "none", and the
// Responses API keeps the effort.
func WithDials(layers ...types.DialLayer) Option {
	return func(c *config) {
		for _, l := range layers {
			c.params.dials = append(c.params.dials, l.Clone())
		}
	}
}

// WithDialPolicy sets how dials the model cannot honor are handled. A
// request's own policy replaces it.
func WithDialPolicy(p types.DialPolicy) Option {
	return func(c *config) {
		p = p.Clone()
		c.params.dialPolicy = &p
	}
}

// DialSurface implements types.DialSurfaceReporter.
func (a *Adapter) DialSurface() string { return types.SurfaceChat }

// DialSurface implements types.DialSurfaceReporter.
func (r *ResponsesAdapter) DialSurface() string { return types.SurfaceResponses }

// compileDials returns a copy of the adapter with its configured dials and
// those of o compiled for one request and applied as options. The copy has
// no dials left. Without dials it returns a unchanged.
func (a *Adapter) compileDials(o types.RequestOptions, tools []types.ToolDef, schema bool, surface string) (*Adapter, error) {
	all := a.EffectiveOptions().Merge(types.RequestOptions{Dials: o.Dials, DialLayers: o.DialLayers, DialPolicy: o.DialPolicy})
	if !all.HasDials() {
		return a, nil
	}
	eff, _, err := types.CompileOptions(a.Capabilities(), all, types.DialContext{Tools: len(tools) > 0, Schema: schema, Surface: surface})
	if err != nil {
		return nil, err
	}
	c, err := a.withRequestOptions(eff)
	if err != nil {
		return nil, err
	}
	c.params.dials, c.params.dialPolicy = nil, nil
	return c, nil
}
