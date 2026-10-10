package types

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
)

// ParamName names a request parameter in a ParamSpace: "temperature",
// "reasoning.effort", "service_tier" and so on.
type ParamName string

// Parameter names for the controls RequestOptions carries.
const (
	ParamTemperature      ParamName = "temperature"
	ParamTopP             ParamName = "top_p"
	ParamTopK             ParamName = "top_k"
	ParamFrequencyPenalty ParamName = "frequency_penalty"
	ParamPresencePenalty  ParamName = "presence_penalty"
	ParamSeed             ParamName = "seed"
	ParamMaxOutputTokens  ParamName = "max_output_tokens"
	ParamStop             ParamName = "stop"
	ParamParallelTools    ParamName = "parallel_tools"
	ParamToolChoice       ParamName = "tool_choice"
	ParamReasoningEnabled ParamName = "reasoning.enabled"
	ParamReasoningEffort  ParamName = "reasoning.effort"
	ParamReasoningBudget  ParamName = "reasoning.budget"
	ParamServiceTier      ParamName = "service_tier"
)

// Parameter types.
const (
	ParamTypeNumber     = "number"
	ParamTypeInteger    = "integer"
	ParamTypeBoolean    = "boolean"
	ParamTypeEnum       = "enum"
	ParamTypeStringList = "string_list"
)

// Request shape keys a Condition may test.
const (
	CondRequestTools   = "request.tools"
	CondRequestSchema  = "request.schema"
	CondRequestSurface = "request.surface"
)

// ParamSpec declares one parameter an offering accepts.
type ParamSpec struct {
	Type    string   `json:"type,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Values  []string `json:"values,omitempty"`
	Default any      `json:"default,omitempty"`
	// Allowed false declares the parameter unsupported. An undeclared
	// parameter is rejected the same way.
	Allowed *bool `json:"allowed,omitempty"`
	// Required means the parameter cannot be turned off, such as reasoning
	// on a model that always reasons.
	Required bool `json:"required,omitempty"`
	// Special maps values outside the range to their meaning, such as
	// {"-1": "dynamic", "0": "off"} for a reasoning budget.
	Special map[string]string `json:"special,omitempty"`
	// Wire is the vendor's field name, for documentation.
	Wire string `json:"wire,omitempty"`
}

// Accepted reports whether the parameter may be sent.
func (s ParamSpec) Accepted() bool { return s.Allowed == nil || *s.Allowed }

// CondValue is a test on one parameter or request-shape key. In and Not
// test the value as a string and Bool tests a boolean, all at the
// parameter's default when the request leaves it unset; Set tests whether
// the request sets it explicitly.
type CondValue struct {
	In   []string `json:"in,omitempty"`
	Not  []string `json:"not,omitempty"`
	Bool *bool    `json:"bool,omitempty"`
	Set  *bool    `json:"set,omitempty"`
}

// Condition holds when every key's test passes. Keys are parameter names or
// request-shape keys.
type Condition map[string]CondValue

// Constraint restricts parameters together. When it holds (an empty When
// always holds), Forbid lists parameters that must not be set, Require
// limits parameters to listed values, and Exclusive lists parameters of
// which at most one may be set.
type Constraint struct {
	When      Condition              `json:"when,omitempty"`
	Forbid    []ParamName            `json:"forbid,omitempty"`
	Require   map[ParamName][]string `json:"require,omitempty"`
	Exclusive []ParamName            `json:"exclusive,omitempty"`
	Reason    string                 `json:"reason,omitempty"`
}

// ParamSpace is the parameter specification of an offering: what may be
// sent, in what range, and in what combinations.
type ParamSpace struct {
	Params      map[ParamName]ParamSpec `json:"params,omitempty"`
	Constraints []Constraint            `json:"constraints,omitempty"`
}

// RequestShape is what a request carries besides its options. Provider and
// Model, when set, name the target in errors.
type RequestShape struct {
	Tools    bool
	Schema   bool
	Surface  string
	Provider string
	Model    string
}

// paramValue is one parameter as a request sets it.
type paramValue struct {
	set    bool
	num    float64
	isNum  bool
	str    string
	isBool bool
	b      bool
}

func (v paramValue) String() string {
	switch {
	case v.isBool:
		return strconv.FormatBool(v.b)
	case v.isNum:
		return strconv.FormatFloat(v.num, 'g', -1, 64)
	default:
		return v.str
	}
}

func numParam(p *float64) paramValue {
	if p == nil {
		return paramValue{}
	}
	return paramValue{set: true, num: *p, isNum: true}
}

func intParam(p *int64) paramValue {
	if p == nil {
		return paramValue{}
	}
	return paramValue{set: true, num: float64(*p), isNum: true}
}

func boolParam(p *bool) paramValue {
	if p == nil {
		return paramValue{}
	}
	return paramValue{set: true, isBool: true, b: *p}
}

// requestParams lists the parameters o sets, by name.
func requestParams(o RequestOptions) map[ParamName]paramValue {
	m := map[ParamName]paramValue{
		ParamTemperature:      numParam(o.Temperature),
		ParamTopP:             numParam(o.TopP),
		ParamTopK:             numParam(o.TopK),
		ParamFrequencyPenalty: numParam(o.FrequencyPenalty),
		ParamPresencePenalty:  numParam(o.PresencePenalty),
		ParamSeed:             intParam(o.Seed),
		ParamMaxOutputTokens:  intParam(o.MaxOutputTokens),
		ParamParallelTools:    boolParam(o.ParallelTools),
		ParamReasoningEnabled: boolParam(o.ReasoningEnabled),
		ParamReasoningBudget:  intParam(o.ReasoningBudget),
	}
	if len(o.StopSequences) > 0 {
		m[ParamStop] = paramValue{set: true, str: fmt.Sprint(o.StopSequences)}
	}
	if o.ReasoningEffort != nil {
		m[ParamReasoningEffort] = paramValue{set: true, str: *o.ReasoningEffort}
	}
	if o.ToolChoice != nil && o.ToolChoice.Mode != "" && o.ToolChoice.Mode != ToolChoiceAuto {
		m[ParamToolChoice] = paramValue{set: true, str: string(o.ToolChoice.Mode)}
	}
	return m
}

// legacyOptionName is the name the request validation errors have always
// used for a parameter, so a ParamSpace rejects with the same text.
func legacyOptionName(p ParamName, unsupported bool) string {
	if unsupported {
		switch p {
		case ParamStop:
			return string(CapStopSequences)
		case ParamParallelTools:
			return string(CapParallelToolControl)
		case ParamReasoningEffort:
			return string(CapReasoningEffort)
		case ParamReasoningBudget:
			return string(CapReasoningBudget)
		case ParamReasoningEnabled:
			return string(CapReasoningToggle)
		}
		return string(p)
	}
	switch p {
	case ParamReasoningEffort:
		return "reasoning_effort"
	case ParamReasoningBudget:
		return "reasoning_budget"
	case ParamReasoningEnabled:
		return "reasoning_enabled"
	}
	return string(p)
}

// paramOrder is the order Validate checks parameters in, which fixes which
// error a request with several problems reports.
var paramOrder = []ParamName{
	ParamTemperature, ParamTopP, ParamTopK, ParamSeed, ParamMaxOutputTokens, ParamFrequencyPenalty,
	ParamPresencePenalty, ParamStop, ParamParallelTools, ParamReasoningEffort, ParamReasoningBudget,
	ParamReasoningEnabled, ParamToolChoice,
}

// Validate checks o against the space: every parameter set must be
// declared and allowed, within its range or values, and every constraint
// that holds for the request must be met. Errors match
// ErrInvalidModelConfig and name the parameter, never the prompt (D-12).
func (s ParamSpace) Validate(o RequestOptions, shape RequestShape) error {
	fail := func(option, reason string) error {
		err := fmt.Errorf("%w: %s: %s", ErrInvalidModelConfig, option, reason)
		if shape.Provider != "" || shape.Model != "" {
			return &ProviderError{Provider: shape.Provider, Model: shape.Model, Kind: ErrorKindPermanent, Err: err}
		}
		return err
	}
	set := requestParams(o)
	for _, name := range paramOrder {
		v := set[name]
		if !v.set {
			continue
		}
		spec, ok := s.Params[name]
		if !ok || !spec.Accepted() {
			if name == ParamToolChoice {
				return fail(string(CapToolChoice), "not declared supported for this model")
			}
			return fail(legacyOptionName(name, true), "not declared supported for this model")
		}
	}
	for _, name := range paramOrder {
		v := set[name]
		if !v.set {
			continue
		}
		if err := s.Params[name].check(name, v, fail); err != nil {
			return err
		}
	}
	// A constraint may forbid a request-shape key, such as tools on an API
	// that cannot take them for this model.
	if shape.Tools {
		set[CondRequestTools] = paramValue{set: true, isBool: true, b: true}
	}
	if shape.Schema {
		set[CondRequestSchema] = paramValue{set: true, isBool: true, b: true}
	}
	for _, c := range s.Constraints {
		if !s.holds(c.When, set, shape) {
			continue
		}
		if err := c.check(set, fail); err != nil {
			return err
		}
	}
	return nil
}

func (spec ParamSpec) check(name ParamName, v paramValue, fail func(string, string) error) error {
	opt := legacyOptionName(name, false)
	if spec.Type == ParamTypeEnum && !slices.Contains(spec.Values, v.str) {
		if name == ParamToolChoice {
			return fail("tool_choice", "this model rejects a forced tool choice (required or named)")
		}
		return fail(opt, fmt.Sprintf("must be one of %v", spec.Values))
	}
	if spec.Required && (v.isBool && !v.b || name == ParamReasoningEffort && v.str == reasoningEffortNone) {
		return fail(opt, "reasoning cannot be disabled")
	}
	if _, ok := spec.Special[v.String()]; ok {
		if spec.Required && v.String() == "0" {
			return fail(opt, "outside the declared range or disables required reasoning")
		}
		return nil
	}
	if !v.isNum || spec.Type != ParamTypeNumber && spec.Type != ParamTypeInteger {
		return nil
	}
	if name == ParamMaxOutputTokens || name == ParamReasoningBudget {
		if v.num <= 0 || spec.Min != nil && v.num < *spec.Min || spec.Max != nil && v.num > *spec.Max {
			if name == ParamMaxOutputTokens {
				return fail(opt, "must be positive and within the declared model limit")
			}
			return fail(opt, "outside the declared range or disables required reasoning")
		}
		return nil
	}
	lo, hi := math.Inf(-1), math.Inf(1)
	if spec.Min != nil {
		lo = *spec.Min
	}
	if spec.Max != nil {
		hi = *spec.Max
	}
	if math.IsNaN(v.num) || math.IsInf(v.num, 0) || v.num < lo || v.num > hi {
		if math.IsInf(hi, 1) {
			hi = math.MaxFloat64
		}
		return fail(opt, fmt.Sprintf("must be finite and in [%g, %g]", lo, hi))
	}
	return nil
}

// holds evaluates a condition. An unset parameter is tested at its spec's
// default (a required boolean at true), except by Set, which tests what the
// request sets.
func (s ParamSpace) holds(c Condition, set map[ParamName]paramValue, shape RequestShape) bool {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		test := c[k]
		var v paramValue
		explicit := true
		switch k {
		case CondRequestTools:
			v = paramValue{set: true, isBool: true, b: shape.Tools}
		case CondRequestSchema:
			v = paramValue{set: true, isBool: true, b: shape.Schema}
		case CondRequestSurface:
			v = paramValue{set: shape.Surface != "", str: shape.Surface}
		default:
			v = set[ParamName(k)]
			explicit = v.set
			if !v.set {
				spec := s.Params[ParamName(k)]
				v = defaultValue(spec.Default)
				if spec.Required && spec.Type == ParamTypeBoolean && !v.set {
					// Reasoning that cannot be turned off is on.
					v = paramValue{set: true, isBool: true, b: true}
				}
			}
		}
		if !test.match(v, explicit) {
			return false
		}
	}
	return true
}

func defaultValue(d any) paramValue {
	switch x := d.(type) {
	case nil:
		return paramValue{}
	case bool:
		return paramValue{set: true, isBool: true, b: x}
	case float64:
		return paramValue{set: true, isNum: true, num: x}
	case int:
		return paramValue{set: true, isNum: true, num: float64(x)}
	case int64:
		return paramValue{set: true, isNum: true, num: float64(x)}
	case string:
		if x == "" {
			return paramValue{}
		}
		return paramValue{set: true, str: x}
	default:
		return paramValue{set: true, str: fmt.Sprint(x)}
	}
}

func (t CondValue) match(v paramValue, explicit bool) bool {
	if t.Set != nil && *t.Set != explicit {
		return false
	}
	if t.Bool != nil && (!v.set || !v.isBool || v.b != *t.Bool) {
		return false
	}
	if len(t.In) > 0 && (!v.set || !slices.Contains(t.In, v.String())) {
		return false
	}
	if len(t.Not) > 0 && (!v.set || slices.Contains(t.Not, v.String())) {
		return false
	}
	return true
}

func (c Constraint) check(set map[ParamName]paramValue, fail func(string, string) error) error {
	if len(c.Exclusive) > 1 {
		n := 0
		for _, p := range c.Exclusive {
			if set[p].set {
				n++
			}
		}
		if n > 1 {
			return fail(exclusiveName(c.Exclusive), c.reason("use only one of toggle, budget or effort"))
		}
	}
	for _, p := range c.Forbid {
		if set[p].set {
			return fail(legacyOptionName(p, true), c.reason("requires reasoning to be disabled"))
		}
	}
	names := make([]ParamName, 0, len(c.Require))
	for p := range c.Require {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		if v := set[p]; v.set && !slices.Contains(c.Require[p], v.String()) {
			return fail(legacyOptionName(p, false), c.reason(fmt.Sprintf("must be one of %v", c.Require[p])))
		}
	}
	return nil
}

func (c Constraint) reason(fallback string) string {
	if c.Reason != "" {
		return c.Reason
	}
	return fallback
}

// exclusiveName names an exclusive group in errors: the reasoning
// controls by their legacy name "reasoning", any other group by its first
// member.
func exclusiveName(group []ParamName) string {
	for _, p := range group {
		switch p {
		case ParamReasoningEffort, ParamReasoningBudget, ParamReasoningEnabled:
		default:
			return string(group[0])
		}
	}
	return "reasoning"
}

// Intersect returns the parameters both spaces accept, with ranges
// narrowed to their overlap, enum values intersected, and every constraint
// of both kept. It is the space a request must fit to be valid for either
// target.
func (s ParamSpace) Intersect(o ParamSpace) ParamSpace {
	out := ParamSpace{Params: map[ParamName]ParamSpec{}}
	for name, a := range s.Params {
		b, ok := o.Params[name]
		if !ok || !a.Accepted() || !b.Accepted() {
			continue
		}
		out.Params[name] = a.intersect(b)
	}
	out.Constraints = append(slices.Clone(s.Constraints), o.Constraints...)
	return out
}

func (s ParamSpec) intersect(o ParamSpec) ParamSpec {
	out := s
	out.Min = maxPtr(s.Min, o.Min)
	out.Max = minPtr(s.Max, o.Max)
	if s.Values != nil || o.Values != nil {
		var vs []string
		for _, v := range s.Values {
			if slices.Contains(o.Values, v) {
				vs = append(vs, v)
			}
		}
		out.Values = vs
	}
	out.Required = s.Required || o.Required
	if s.Special != nil || o.Special != nil {
		out.Special = map[string]string{}
		for k, v := range s.Special {
			if _, ok := o.Special[k]; ok {
				out.Special[k] = v
			}
		}
	}
	return out
}

func maxPtr(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a >= *b:
		return a
	default:
		return b
	}
}

func minPtr(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a <= *b:
		return a
	default:
		return b
	}
}
