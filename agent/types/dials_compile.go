package types

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// DialMap is how one model compiles dials to raw options. The catalog fills
// it from a row's dials object; a part left nil is derived from the row's
// other declarations by EffectiveDialMap.
type DialMap struct {
	Creativity *CreativityMap
	Reasoning  *ReasoningMap
	// CacheMode is the prompt cache mode the cache dial turns on, such as
	// "markers" or "automatic". Empty derives it from the provider.
	CacheMode string
	// Defaults are the model's own dials. The catalog and provider.Build
	// apply them below every other layer except the global one.
	Defaults Dials
}

// CreativityMap maps each creativity level to sampling options.
type CreativityMap struct {
	Levels map[Creativity]RequestOptions
	// RequiresReasoningOff drops creativity whenever reasoning is active, for
	// APIs that reject sampling controls while the model thinks.
	RequiresReasoningOff bool
}

// DepthChangePerRequest declares that a model accepts a new reasoning depth
// on any request, even inside a tool loop with signed reasoning.
const DepthChangePerRequest = "per_request"

// ReasoningMap maps the reasoning dial to reasoning options. A nil Off,
// On or Adaptive means the mode cannot be expressed; an empty value means
// it is expressed by sending nothing.
type ReasoningMap struct {
	Off, On, Adaptive *RequestOptions
	// Depth maps each declared depth. A depth missing here maps to the
	// nearest declared one.
	Depth map[Depth]RequestOptions
	// DepthChange is DepthChangePerRequest, or empty when a depth change
	// waits for the next user turn while signed reasoning is open.
	DepthChange string
	// ChangeResetsCache declares that changing reasoning invalidates the
	// provider's cached prompt prefix.
	ChangeResetsCache bool
	// WithTools replaces the compiled reasoning when the request offers
	// tools, keyed by API surface, for an API that takes tools only with
	// a particular reasoning setting.
	WithTools map[string]RequestOptions
}

// Clone returns a deep copy.
func (m DialMap) Clone() DialMap {
	out := DialMap{CacheMode: m.CacheMode, Defaults: m.Defaults.Clone()}
	if m.Creativity != nil {
		c := CreativityMap{Levels: cloneOptionMap(m.Creativity.Levels), RequiresReasoningOff: m.Creativity.RequiresReasoningOff}
		out.Creativity = &c
	}
	if r := m.Reasoning; r != nil {
		c := ReasoningMap{Off: cloneOptionsPtr(r.Off), On: cloneOptionsPtr(r.On), Adaptive: cloneOptionsPtr(r.Adaptive),
			Depth: cloneOptionMap(r.Depth), DepthChange: r.DepthChange, ChangeResetsCache: r.ChangeResetsCache,
			WithTools: cloneOptionMap(r.WithTools)}
		out.Reasoning = &c
	}
	return out
}

// IsZero reports whether nothing is declared.
func (m DialMap) IsZero() bool {
	return m.Creativity == nil && m.Reasoning == nil && m.CacheMode == "" && m.Defaults.IsZero()
}

func cloneOptionMap[K comparable](in map[K]RequestOptions) map[K]RequestOptions {
	if in == nil {
		return nil
	}
	out := make(map[K]RequestOptions, len(in))
	for k, v := range in {
		out[k] = v.Clone()
	}
	return out
}

func cloneOptionsPtr(p *RequestOptions) *RequestOptions {
	if p == nil {
		return nil
	}
	c := p.Clone()
	return &c
}

// EffectiveDialMap returns the declared dial map with every part it leaves
// unset derived from the other declarations:
//   - creativity from temperature support, with reasoning conflicts taken
//     from SamplingRequiresNoReasoning;
//   - reasoning depth from the effort list by name, from a budget range by
//     fraction, or from a toggle as on and off;
//   - the cache mode from the provider's prompt cache capability.
func (mc ModelCapabilities) EffectiveDialMap() DialMap {
	d := mc.DialMap.Clone()
	if c, derived := d.Creativity, deriveCreativity(mc); c == nil {
		d.Creativity = derived
	} else if len(c.Levels) == 0 && derived != nil {
		c.Levels = derived.Levels
		c.RequiresReasoningOff = c.RequiresReasoningOff || derived.RequiresReasoningOff
	}
	derived := deriveReasoning(mc)
	if r := d.Reasoning; r == nil {
		d.Reasoning = derived
	} else if derived != nil {
		if r.Off == nil {
			r.Off = derived.Off
		}
		if r.On == nil {
			r.On = derived.On
		}
		if r.Adaptive == nil {
			r.Adaptive = derived.Adaptive
		}
		if r.Depth == nil {
			r.Depth = derived.Depth
		}
		if r.WithTools == nil {
			r.WithTools = derived.WithTools
		}
	}
	if d.CacheMode == "" {
		switch {
		case mc.Provider == providerAnthropic && mc.Supports(CapPromptCacheMarkers):
			d.CacheMode = "markers"
		case mc.Provider == providerOpenAI && mc.Supports(CapAutomaticPromptCache):
			d.CacheMode = "automatic"
		}
	}
	return d
}

// Provider names whose prompt cache mode the cache dial derives.
const (
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
)

func f64p(v float64) *float64 { return &v }
func i64p(v int64) *int64     { return &v }
func strp(v string) *string   { return &v }
func boolp(v bool) *bool      { return &v }

func deriveCreativity(mc ModelCapabilities) *CreativityMap {
	if !mc.Supports(CapTemperature) {
		return nil
	}
	focused := RequestOptions{Temperature: f64p(0.3)}
	if mc.Supports(CapTopP) {
		focused.TopP = f64p(0.9)
	}
	return &CreativityMap{
		Levels: map[Creativity]RequestOptions{
			CreativityDeterministic: {Temperature: f64p(0)},
			CreativityFocused:       focused,
			CreativityBalanced:      {Temperature: f64p(0.7)},
			CreativityCreative:      {Temperature: f64p(1)},
		},
		RequiresReasoningOff: len(mc.SamplingRequiresNoReasoning) > 0,
	}
}

// depthFraction places each depth in a declared budget range.
var depthFraction = map[Depth]float64{DepthMinimal: 0, DepthLow: 0.25, DepthMedium: 0.5, DepthHigh: 0.75, DepthMax: 1}

// depthBudget is the budget of each depth when no maximum is declared.
var depthBudget = map[Depth]int64{DepthMinimal: 1024, DepthLow: 2048, DepthMedium: 8192, DepthHigh: 16384, DepthMax: 32768}

func deriveReasoning(mc ModelCapabilities) *ReasoningMap {
	defaultOn := mc.ReasoningActive(RequestOptions{})
	empty := func() *RequestOptions { return &RequestOptions{} }
	m := &ReasoningMap{}
	switch {
	case mc.Supports(CapReasoningEffort) && len(mc.ReasoningEfforts) > 0:
		m.Depth = map[Depth]RequestOptions{}
		for _, d := range depthOrder {
			if slices.Contains(mc.ReasoningEfforts, string(d)) {
				m.Depth[d] = RequestOptions{ReasoningEffort: strp(string(d))}
			}
		}
		hasNone := slices.Contains(mc.ReasoningEfforts, reasoningEffortNone)
		if hasNone && !mc.ReasoningRequired {
			m.Off = &RequestOptions{ReasoningEffort: strp(reasoningEffortNone)}
		}
		if defaultOn {
			m.Adaptive = empty()
		}
		if mc.ChatCompletionsTools == ChatToolsNoReasoning && hasNone {
			m.WithTools = map[string]RequestOptions{SurfaceChat: {ReasoningEffort: strp(reasoningEffortNone)}}
		}
	case mc.Supports(CapReasoningBudget):
		m.Depth = map[Depth]RequestOptions{}
		for _, d := range depthOrder {
			n := max(depthBudget[d], int64(mc.MinReasoningBudget))
			if hi := int64(mc.MaxReasoningBudget); hi > 0 {
				lo := max(int64(mc.MinReasoningBudget), 1)
				n = lo + int64(depthFraction[d]*float64(hi-lo))
			}
			m.Depth[d] = RequestOptions{ReasoningBudget: i64p(n)}
		}
		if mc.ZeroReasoningBudget && !mc.ReasoningRequired {
			m.Off = &RequestOptions{ReasoningBudget: i64p(0)}
		}
		switch {
		case mc.DynamicReasoningBudget:
			m.Adaptive = &RequestOptions{ReasoningBudget: i64p(-1)}
		case defaultOn:
			m.Adaptive = empty()
		}
	case mc.Supports(CapReasoningToggle):
		m.On = &RequestOptions{ReasoningEnabled: boolp(true)}
		if !mc.ReasoningRequired {
			m.Off = &RequestOptions{ReasoningEnabled: boolp(false)}
		}
	}
	if m.Off == nil && !defaultOn {
		// Reasoning is already off by default: nothing needs sending.
		m.Off = empty()
	}
	return m
}

// outcome is how one dial compiles, before the policy applies.
type outcome struct {
	action DialAction
	opts   RequestOptions
	reason string
}

func applied(o RequestOptions) outcome { return outcome{action: DialApplied, opts: o} }
func mapped(o RequestOptions, reason string) outcome {
	return outcome{action: DialMapped, opts: o, reason: reason}
}
func dropped(reason string) outcome { return outcome{action: DialDropped, reason: reason} }

// hold is a reasoning change an open signed tool loop defers.
type hold struct {
	want, keep ReasoningDial
}

type compiler struct {
	mc     ModelCapabilities
	dm     DialMap
	raw    RequestOptions
	ctx    DialContext
	pol    DialPolicy
	dials  Dials
	scopes map[DialName]string
	hold   *hold
	out    RequestOptions
	rep    *DialReport
}

// ResolveDials compiles dials against one model. It is pure and
// deterministic: adapters, routers and catalog validation call it with the
// same inputs and get the same result.
//
// Layers apply in order, each overriding the dials of the ones before it.
// raw holds the raw options already in force, the adapter's configured
// options with the request's on top; a raw option that sets the same
// parameter as a dial wins, is recorded as raw_override, and is validated
// strictly. Each dial a model cannot honor exactly is mapped, dropped or
// rejected as pol says. Reasoning compiles before creativity, and when the
// effective reasoning rules out sampling controls, creativity is dropped.
//
// The returned options carry no dials. They pass mc.ValidateOptions, or
// the error matches ErrInvalidModelConfig and the report records the
// decisions made before it.
func ResolveDials(mc ModelCapabilities, raw RequestOptions, ctx DialContext, pol DialPolicy, layers ...DialLayer) (RequestOptions, DialReport, error) {
	raw = raw.Raw()
	c := &compiler{mc: mc, dm: mc.EffectiveDialMap(), raw: raw, ctx: ctx, pol: pol.Clone(),
		scopes: map[DialName]string{}, out: raw.Clone()}
	for _, l := range layers {
		c.dials = c.dials.Merge(l.Dials)
		for _, n := range l.Dials.Names() {
			c.scopes[n] = l.Scope
		}
		if l.Dials.Reasoning != nil {
			c.hold = nil
			if l.Hold != nil && l.Hold.normalized() != l.Dials.Reasoning.normalized() {
				c.hold = &hold{want: *l.Dials.Reasoning, keep: *l.Hold}
			}
		}
	}
	rep := DialReport{Requested: c.dials.Clone(), Policy: pol.String()}
	c.rep = &rep
	if err := c.dials.Validate(); err != nil {
		return RequestOptions{}, rep, err
	}
	if err := pol.Validate(); err != nil {
		return RequestOptions{}, rep, err
	}
	for _, step := range []func() error{c.maxOutput, c.reasoning, c.creativity, c.tools, c.parallel, c.seed, c.cache} {
		if err := step(); err != nil {
			c.finish()
			return RequestOptions{}, rep, err
		}
	}
	c.finish()
	if err := mc.ValidateOptions(c.out); err != nil {
		return RequestOptions{}, rep, err
	}
	rep.Effective = c.out.Clone()
	rep.EffectiveHash = optionsHash(c.out)
	return c.out.Clone(), rep, nil
}

// finish orders the decisions canonically.
func (c *compiler) finish() {
	order := AllDialNames()
	slices.SortStableFunc(c.rep.Decisions, func(a, b DialDecision) int {
		return slices.Index(order, a.Dial) - slices.Index(order, b.Dial)
	})
}

// settle applies the policy to one dial's outcome and records it.
func (c *compiler) settle(name DialName, requested string, o outcome, deferred bool) error {
	d := DialDecision{Dial: name, Requested: requested, Action: o.action, Reason: o.reason, Scope: c.scopes[name]}
	if o.action != DialDropped {
		d.Sent = describeOptions(o.opts)
	}
	if o.action != DialApplied {
		switch c.pol.Handling(name, c.dials) {
		case HandlingReject:
			d.Action, d.Sent = DialRejected, ""
			c.rep.Decisions = append(c.rep.Decisions, d)
			return c.mc.OptionError("dial "+string(name), o.reason)
		case HandlingDrop:
			if o.action == DialMapped {
				d.Action, d.Sent = DialDropped, ""
				d.Reason = "the policy drops instead of mapping: " + o.reason
			}
		}
	}
	if d.Action == DialApplied || d.Action == DialMapped {
		c.out = c.out.Merge(o.opts)
	}
	if deferred {
		d.Action = DialDeferred
	}
	c.rep.Decisions = append(c.rep.Decisions, d)
	return nil
}

// override records a dial whose parameters a raw option already sets.
func (c *compiler) override(name DialName, requested string, sent RequestOptions) {
	c.rep.Decisions = append(c.rep.Decisions, DialDecision{Dial: name, Requested: requested, Sent: describeOptions(sent),
		Action: DialRawOverride, Reason: "a raw option sets the same parameter", Scope: c.scopes[name]})
}

func (c *compiler) maxOutput() error {
	v := c.dials.MaxOutput
	if v == nil {
		return nil
	}
	req := strconv.FormatInt(*v, 10)
	if r := c.raw.MaxOutputTokens; r != nil && (c.mc.DefaultMaxOutputTokens <= 0 || *r != int64(c.mc.DefaultMaxOutputTokens)) {
		// A raw value equal to the declared default is the adapter's
		// default rather than a caller's choice, so the dial replaces it.
		c.override(DialMaxOutput, req, RequestOptions{MaxOutputTokens: r})
		return nil
	}
	var o outcome
	switch ceiling := int64(c.mc.MaxOutputTokens); {
	case !c.mc.Supports(CapMaxOutputTokens):
		o = dropped("the model takes no output token cap")
	case ceiling > 0 && *v > ceiling:
		o = mapped(RequestOptions{MaxOutputTokens: i64p(ceiling)}, fmt.Sprintf("lowered to the model ceiling %d", ceiling))
	default:
		o = applied(RequestOptions{MaxOutputTokens: i64p(*v)})
	}
	return c.settle(DialMaxOutput, req, o, false)
}

func (c *compiler) reasoning() error {
	r := c.dials.Reasoning
	if r == nil {
		return nil
	}
	if c.raw.HasReasoning() {
		c.override(DialReasoning, r.String(), c.raw)
		return nil
	}
	want, deferred, reason := *r, false, ""
	if h := c.hold; h != nil {
		perRequest := c.dm.Reasoning != nil && c.dm.Reasoning.DepthChange == DepthChangePerRequest
		if h.want.normalized().Mode != h.keep.normalized().Mode {
			want, deferred, reason = h.keep, true, "a reasoning mode change waits for the next user turn while a signed tool loop is open"
		} else if !perRequest {
			want, deferred, reason = h.keep, true, "a depth change waits for the next user turn on this model while a signed tool loop is open"
		}
	}
	o := c.compileReasoning(want)
	if deferred {
		o.reason = joinReason(reason, o.reason)
	}
	if err := c.settle(DialReasoning, r.String(), o, deferred); err != nil {
		return err
	}
	if prev := c.ctx.Previous; prev != nil && c.dm.Reasoning != nil && c.dm.Reasoning.ChangeResetsCache &&
		describeReasoning(*prev) != describeReasoning(c.out) {
		c.rep.Decisions[len(c.rep.Decisions)-1].CacheResetExpected = true
	}
	return nil
}

func joinReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// compileReasoning maps a reasoning dial through the reasoning map.
func (c *compiler) compileReasoning(r ReasoningDial) outcome {
	rm := c.dm.Reasoning
	if rm == nil {
		return dropped("the model declares no reasoning controls")
	}
	r = r.normalized()
	var o outcome
	switch {
	case r.Mode == ReasoningOff:
		switch {
		case rm.Off != nil:
			o = applied(*rm.Off)
		default:
			if d, ok := nearestDepth(rm.Depth, DepthMinimal); ok {
				o = mapped(rm.Depth[d], "reasoning cannot be turned off on this model; sent the lowest declared depth")
			} else {
				o = dropped("reasoning cannot be turned off on this model")
			}
		}
	case r.Mode == ReasoningAdaptive && rm.Adaptive != nil:
		o = applied(*rm.Adaptive)
	case r.Mode == ReasoningAdaptive:
		o = c.depth(rm, DepthMedium)
		if o.action != DialDropped {
			o.action, o.reason = DialMapped, joinReason("adaptive reasoning is not declared; sent depth medium", o.reason)
		}
	case r.Depth == "" && rm.On != nil:
		o = applied(*rm.On)
	case r.Depth == "":
		o = c.depth(rm, DepthMedium)
	default:
		o = c.depth(rm, r.Depth)
	}
	if o.action == DialDropped || r.Mode == ReasoningOff {
		return o
	}
	if alt, ok := rm.WithTools[c.ctx.Surface]; ok && c.ctx.Tools && c.mc.ReasoningActive(o.opts) {
		return mapped(alt, "this API takes tools only with "+describeReasoning(alt)+"; another API surface keeps the depth")
	}
	return c.boundBudget(o)
}

// depth maps one depth: exactly, to the nearest declared depth, or to the
// on toggle.
func (c *compiler) depth(rm *ReasoningMap, d Depth) outcome {
	if v, ok := rm.Depth[d]; ok {
		return applied(v)
	}
	if n, ok := nearestDepth(rm.Depth, d); ok {
		return mapped(rm.Depth[n], fmt.Sprintf("depth %s is not declared; sent the nearest, %s", d, n))
	}
	if rm.On != nil {
		return mapped(*rm.On, "depth collapsed to toggle")
	}
	return dropped("the model declares no reasoning depth")
}

// boundBudget keeps a budget below the output cap, which Anthropic requires.
func (c *compiler) boundBudget(o outcome) outcome {
	b, limit := o.opts.ReasoningBudget, c.out.MaxOutputTokens
	if b == nil || *b <= 0 || limit == nil || *b < *limit {
		return o
	}
	n := *limit / 2
	if n < int64(c.mc.MinReasoningBudget) || n <= 0 {
		return dropped(fmt.Sprintf("no reasoning budget fits max_output_tokens %d", *limit))
	}
	o.opts = RequestOptions{ReasoningBudget: i64p(n)}
	o.action, o.reason = DialMapped, joinReason(o.reason, fmt.Sprintf("budget bounded by max_output_tokens %d", *limit))
	return o
}

// nearestDepth returns the declared depth closest to d; ties go to the
// lower depth.
func nearestDepth(declared map[Depth]RequestOptions, d Depth) (Depth, bool) {
	want, best, bestDist := slices.Index(depthOrder, d), Depth(""), -1
	for i, cand := range depthOrder {
		if _, ok := declared[cand]; !ok {
			continue
		}
		dist := i - want
		if dist < 0 {
			dist = -dist
		}
		if bestDist < 0 || dist < bestDist {
			best, bestDist = cand, dist
		}
	}
	return best, bestDist >= 0
}

func (c *compiler) creativity() error {
	v := c.dials.Creativity
	if v == nil {
		return nil
	}
	req := string(*v)
	if c.raw.Temperature != nil || c.raw.TopP != nil || c.raw.TopK != nil {
		c.override(DialCreativity, req, RequestOptions{Temperature: c.raw.Temperature, TopP: c.raw.TopP, TopK: c.raw.TopK})
		return nil
	}
	return c.settle(DialCreativity, req, c.compileCreativity(*v), false)
}

func (c *compiler) compileCreativity(v Creativity) outcome {
	cm := c.dm.Creativity
	if cm == nil || len(cm.Levels) == 0 {
		return dropped("the model takes no sampling controls")
	}
	o := outcome{action: DialApplied}
	if opts, ok := cm.Levels[v]; ok {
		o.opts = opts
	} else {
		want, best, bestDist := slices.Index(creativityOrder, v), Creativity(""), -1
		for i, cand := range creativityOrder {
			if _, ok := cm.Levels[cand]; !ok {
				continue
			}
			dist := max(i-want, want-i)
			if bestDist < 0 || dist < bestDist {
				best, bestDist = cand, dist
			}
		}
		if bestDist < 0 {
			return dropped("the model declares no creativity level")
		}
		o = mapped(cm.Levels[best], fmt.Sprintf("level %s is not declared; sent the nearest, %s", v, best))
	}
	if c.mc.ReasoningActive(c.out) && (cm.RequiresReasoningOff || c.samplingConflicts(o.opts)) {
		return dropped("conflicts with " + c.reasoningInForce())
	}
	if err := c.mc.ValidateOptions(c.out.Merge(o.opts)); err != nil {
		return dropped(OptionReason(err))
	}
	return o
}

func (c *compiler) samplingConflicts(o RequestOptions) bool {
	for _, item := range []struct {
		cap Capability
		set bool
	}{{CapTemperature, o.Temperature != nil}, {CapTopP, o.TopP != nil}, {CapTopK, o.TopK != nil}} {
		if item.set && slices.Contains(c.mc.SamplingRequiresNoReasoning, item.cap) {
			return true
		}
	}
	return false
}

// reasoningInForce describes the effective reasoning for a conflict reason.
func (c *compiler) reasoningInForce() string {
	if c.out.HasReasoning() {
		return describeReasoning(c.out)
	}
	if e := c.mc.DefaultReasoningEffort; e != "" {
		return "effort " + e
	}
	return "the model's default reasoning"
}

func (c *compiler) tools() error {
	v := c.dials.Tools
	if v == nil {
		return nil
	}
	req := string(v.Mode)
	if v.Name != "" {
		req += ":" + v.Name
	}
	if c.raw.ToolChoice != nil {
		c.override(DialTools, req, RequestOptions{ToolChoice: c.raw.ToolChoice})
		return nil
	}
	choice := *v
	o := applied(RequestOptions{ToolChoice: &choice})
	if err := c.mc.ValidateToolChoice(&choice, nil); err != nil {
		o = dropped(OptionReason(err))
	}
	return c.settle(DialTools, req, o, false)
}

func (c *compiler) parallel() error {
	v := c.dials.Parallel
	if v == nil {
		return nil
	}
	req := strconv.FormatBool(*v)
	if c.raw.ParallelTools != nil {
		c.override(DialParallel, req, RequestOptions{ParallelTools: c.raw.ParallelTools})
		return nil
	}
	o := applied(RequestOptions{ParallelTools: boolp(*v)})
	if !c.mc.Supports(CapParallelToolControl) {
		o = dropped("the model takes no parallel tool control")
	}
	return c.settle(DialParallel, req, o, false)
}

func (c *compiler) seed() error {
	v := c.dials.Seed
	if v == nil {
		return nil
	}
	req := strconv.FormatInt(*v, 10)
	if c.raw.Seed != nil {
		c.override(DialReproducible, req, RequestOptions{Seed: c.raw.Seed})
		return nil
	}
	o := applied(RequestOptions{Seed: i64p(*v)})
	if !c.mc.Supports(CapSeed) {
		o = dropped("the model takes no seed")
	}
	return c.settle(DialReproducible, req, o, false)
}

// cache records the cache dial. The prompt cache is configured when a
// provider is built, so only the layers it is built from can change it.
func (c *compiler) cache() error {
	v := c.dials.Cache
	if v == nil {
		return nil
	}
	req := "off"
	if *v {
		req = "on"
	}
	d := DialDecision{Dial: DialCache, Requested: req, Action: DialApplied, Scope: c.scopes[DialCache]}
	var reason string
	switch {
	case !buildScope(d.Scope):
		reason = "prompt caching is fixed when the provider is built; set the cache dial in the catalog"
	case *v && c.dm.CacheMode == "":
		reason = "the model declares no prompt cache mode"
	case *v:
		d.Sent = "prompt_cache=" + c.dm.CacheMode
	}
	if reason == "" {
		c.rep.Decisions = append(c.rep.Decisions, d)
		return nil
	}
	return c.settle(DialCache, req, dropped(reason), false)
}

// describeOptions renders the raw parameters a dial sent.
func describeOptions(o RequestOptions) string {
	var parts []string
	num := func(name string, v *float64) {
		if v != nil {
			parts = append(parts, name+"="+strconv.FormatFloat(*v, 'g', -1, 64))
		}
	}
	num("temperature", o.Temperature)
	num("top_p", o.TopP)
	num("top_k", o.TopK)
	if o.MaxOutputTokens != nil {
		parts = append(parts, "max_output_tokens="+strconv.FormatInt(*o.MaxOutputTokens, 10))
	}
	if r := describeReasoning(o); r != "" {
		parts = append(parts, strings.Replace(r, " ", "=", 1))
	}
	if o.ToolChoice != nil {
		tc := "tool_choice=" + string(o.ToolChoice.Mode)
		if o.ToolChoice.Name != "" {
			tc += ":" + o.ToolChoice.Name
		}
		parts = append(parts, tc)
	}
	if o.ParallelTools != nil {
		parts = append(parts, "parallel_tools="+strconv.FormatBool(*o.ParallelTools))
	}
	if o.Seed != nil {
		parts = append(parts, "seed="+strconv.FormatInt(*o.Seed, 10))
	}
	return strings.Join(parts, " ")
}

// describeReasoning renders the reasoning control of o, or "".
func describeReasoning(o RequestOptions) string {
	switch {
	case o.ReasoningEffort != nil:
		return "effort " + *o.ReasoningEffort
	case o.ReasoningBudget != nil:
		return "budget " + strconv.FormatInt(*o.ReasoningBudget, 10)
	case o.ReasoningEnabled != nil:
		return "think " + strconv.FormatBool(*o.ReasoningEnabled)
	}
	return ""
}

// OptionReason extracts the reason from an ErrInvalidModelConfig error,
// without the provider, model and option prefixes.
func OptionReason(err error) string {
	var pe *ProviderError
	if errors.As(err, &pe) && pe.Err != nil {
		err = pe.Err
	}
	msg := err.Error()
	if i := strings.Index(msg, ErrInvalidModelConfig.Error()+": "); i >= 0 {
		msg = msg[i+len(ErrInvalidModelConfig.Error())+2:]
	}
	if _, rest, ok := strings.Cut(msg, ": "); ok {
		msg = rest
	}
	return strings.TrimSuffix(msg, " for this model")
}
