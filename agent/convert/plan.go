package convert

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Plan is what one attempt does with the parts of a request so the offering
// that serves it can take them. It is built by PlanConversions without I/O.
type Plan struct {
	// Offering is the ID of the offering the plan targets.
	Offering string
	// Decisions has one entry per media part, and per reasoning part the
	// target cannot verify, in request order.
	Decisions []Decision
	// Estimate is the sum of the planned conversions' estimates.
	Estimate types.ConversionEstimate

	target types.Offering
	policy types.ConversionPolicy
}

// Decision is the plan for one part.
type Decision struct {
	types.ConversionDecision
	// Permitted is the action the dial permitted, empty for a native or
	// lowered part.
	Permitted types.ModalityAction

	part      types.Part
	converter types.Converter
	estimate  types.ConversionEstimate
	// rest are the actions permitted after Action, tried in order when the
	// converter fails.
	rest  []types.ModalityAction
	cause string
}

// Converts reports whether the plan changes the request: anything but
// native or lowered parts.
func (p Plan) Converts() bool {
	return slices.ContainsFunc(p.Decisions, func(d Decision) bool {
		return d.Action != types.DecisionNative && d.Action != types.DecisionLowered
	})
}

// Report is the plan as a report: the decisions it made, before any ran.
func (p Plan) Report() types.ConversionReport {
	r := types.ConversionReport{Offering: p.Offering}
	for _, d := range p.Decisions {
		r.Decisions = append(r.Decisions, d.ConversionDecision)
	}
	r.Hash = r.ComputeHash()
	return r
}

// RejectError reports the parts no permitted action can serve on an
// offering. It matches types.ErrModalityUnsupported (and so
// types.ErrInvalidModelConfig), and types.ErrMediaUnavailable when a
// rejected part had no locator left.
type RejectError struct {
	Offering string
	Rejected []types.ConversionDecision
	// unavailable is set when a rejected part could not be reached at all.
	unavailable bool
}

func (e *RejectError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v on %s:", types.ErrModalityUnsupported, e.Offering)
	for i, d := range e.Rejected {
		if i > 0 {
			b.WriteString(";")
		}
		fmt.Fprintf(&b, " part %s (%s", d.Path, d.Kind)
		if d.MediaType != "" {
			fmt.Fprintf(&b, " %s", d.MediaType)
		}
		fmt.Fprintf(&b, "): %s", d.Reason)
	}
	return b.String()
}

func (e *RejectError) Is(target error) bool {
	return target == types.ErrModalityUnsupported || target == types.ErrInvalidModelConfig ||
		(e.unavailable && target == types.ErrMediaUnavailable)
}

// IsRejected reports whether err is a plan's rejection.
func IsRejected(err error) bool {
	var re *RejectError
	return errors.As(err, &re)
}

// Scopes of the modality dial a decision names when no layer set it.
const (
	// ScopePolicy names the ConversionPolicy's own dial.
	ScopePolicy = "policy"
	// ScopeDefault names the built-in default, reject.
	ScopeDefault = "default"
)

// dial is the effective modality dial with the scope that set each entry.
type dial struct {
	types.ModalityDial
	scope    map[types.Modality]string
	defScope string
}

// resolveDial merges the policy's dial and the layers, lowest first.
func resolveDial(base types.ModalityDial, layers []types.DialLayer) dial {
	d := dial{ModalityDial: base.Clone(), scope: map[types.Modality]string{}, defScope: ScopeDefault}
	if base.Default != "" {
		d.defScope = ScopePolicy
	}
	for m := range base.Per {
		d.scope[m] = ScopePolicy
	}
	for _, l := range layers {
		md := l.Dials.Modality
		if md == nil {
			continue
		}
		d.ModalityDial = d.Merge(*md)
		if md.Default != "" {
			d.defScope = l.Scope
		}
		for m := range md.Per {
			d.scope[m] = l.Scope
		}
	}
	return d
}

// actions returns the permitted actions for m and the scope that set them.
func (d dial) actions(m types.Modality) ([]types.ModalityAction, string) {
	if as, ok := d.Per[m]; ok && len(as) > 0 {
		return slices.Clone(as), d.scope[m]
	}
	return d.Actions(m), d.defScope
}

// PlanConversions decides, for every media part of msgs, whether target
// takes it natively (or lowered from a tool result to a follow-up user
// message, which the adapter does), which permitted action and converter
// fit it to target, or that it is rejected. Reasoning parts the target
// cannot verify are dropped or sent as text, as the policy says. It does no
// I/O: converters are only asked for estimates.
//
// The modality dial is the policy's, with layers applied on top in order.
// A part no permitted action can serve, or a plan whose estimate exceeds the
// policy's MaxCost, makes the error a *RejectError; the plan is still
// returned with every decision.
func PlanConversions(target types.Offering, msgs []types.Message, pol types.ConversionPolicy, layers ...types.DialLayer) (Plan, error) {
	pl := Plan{Offering: target.ID, target: target, policy: pol}
	if pl.Offering == "" {
		pl.Offering = offeringName(target)
	}
	pr := planner{target: target, policy: pol, dial: resolveDial(pol.Dial, layers), counts: map[types.Modality]int{}}
	for mi, msg := range msgs {
		switch v := msg.(type) {
		case types.UserMessage:
			for pi, part := range v.Parts {
				pr.part(&pl, part, types.PartPath{Message: mi, Part: pi, Nested: -1})
			}
		case types.SystemMessage:
			for pi, part := range v.Parts {
				pr.part(&pl, part, types.PartPath{Message: mi, Part: pi, Nested: -1})
			}
		case types.AssistantMessage:
			for pi, part := range v.Parts {
				if t, ok := part.(types.ThinkingPart); ok {
					pr.thinking(&pl, t, types.PartPath{Message: mi, Part: pi, Nested: -1})
				}
			}
		}
	}
	rej := &RejectError{Offering: pl.Offering}
	for _, d := range pl.Decisions {
		if d.Action == types.DecisionRejected {
			rej.Rejected = append(rej.Rejected, d.ConversionDecision)
			rej.unavailable = rej.unavailable || d.cause == causeUnavailable
		}
	}
	if pol.MaxCost > 0 && pl.Estimate.Cost > pol.MaxCost {
		rej.Rejected = append(rej.Rejected, types.ConversionDecision{Path: types.PartPath{Message: -1, Part: -1, Nested: -1},
			Kind: "plan", Action: types.DecisionRejected,
			Reason: fmt.Sprintf("estimated conversion cost %s exceeds the policy cap %s", pl.Estimate.Cost, pol.MaxCost)})
	}
	if len(rej.Rejected) > 0 {
		return pl, rej
	}
	return pl, nil
}

// offeringName names an offering that has no ID, such as one projected
// from capabilities.
func offeringName(o types.Offering) string {
	name := string(o.Model.Vendor)
	if o.Model.Prefix != "" {
		name += "/" + string(o.Model.Prefix)
	}
	if o.Endpoint.Name != "" {
		name += "@" + o.Endpoint.Name
	}
	return name
}

// Causes of a rejection, for the error's matching.
const causeUnavailable = "unavailable"

type planner struct {
	target types.Offering
	policy types.ConversionPolicy
	dial   dial
	// counts are the native parts per modality so far, for MaxCount.
	counts map[types.Modality]int
}

// part plans one top-level part: a media part, or each media part inside a
// tool result.
func (pr *planner) part(pl *Plan, part types.Part, path types.PartPath) {
	if tr, ok := part.(types.ToolResultPart); ok {
		for ni, np := range tr.Parts {
			if types.IsMedia(np) {
				p := path
				p.Nested = ni
				pr.media(pl, np, p, true)
			}
		}
		return
	}
	if types.IsMedia(part) {
		pr.media(pl, part, path, false)
	}
}

func (pr *planner) media(pl *Plan, part types.Part, path types.PartPath, inToolResult bool) {
	src, _ := types.SourceOf(part)
	m, _ := types.PartModality(part)
	d := Decision{part: part, ConversionDecision: types.ConversionDecision{Path: path, Kind: part.Kind(),
		MediaType: src.MediaType, Digest: src.Digest}}
	reason, cause, lowered := pr.native(part, src, m, inToolResult)
	if reason == "" {
		d.Action = types.DecisionNative
		if lowered {
			d.Action = types.DecisionLowered
		}
		pr.counts[m]++
		pl.Decisions = append(pl.Decisions, d)
		return
	}
	actions, scope := pr.dial.actions(m)
	d.Scope = scope
	pr.choose(&d, actions, reason)
	d.cause = cause
	pl.Estimate = addEstimate(pl.Estimate, d.estimate)
	pl.Decisions = append(pl.Decisions, d)
}

// choose sets d to the first of actions that can serve its part, or to a
// rejection. reason says why the part is not native.
func (pr *planner) choose(d *Decision, actions []types.ModalityAction, reason string) {
	var tried []string
	for i, a := range actions {
		switch a {
		case types.ActReject:
			d.Permitted, d.Action, d.Reason = a, types.DecisionRejected, reason
			return
		case types.ActOmit:
			d.Permitted, d.Action, d.Reason = a, types.DecisionOmitted, reason
			return
		}
		c, est, ok := pr.converter(a, d.part)
		if !ok {
			tried = append(tried, string(a))
			continue
		}
		d.Permitted, d.Action = a, decisionFor(a)
		d.converter, d.estimate, d.rest = c, est, slices.Clone(actions[i+1:])
		d.Via = via(c)
		d.Reason = reason
		return
	}
	d.Permitted, d.Action = types.ActReject, types.DecisionRejected
	d.Reason = reason
	if len(tried) > 0 {
		d.Reason += "; no converter for " + strings.Join(tried, ", ")
	}
}

// converter returns the first registered converter for action that accepts
// part and produces something the target takes.
func (pr *planner) converter(action types.ModalityAction, part types.Part) (types.Converter, types.ConversionEstimate, bool) {
	for _, c := range pr.policy.Converters {
		if c == nil || c.Action() != action || !c.Accepts(part) {
			continue
		}
		if t, ok := c.(types.TargetedConverter); ok && !t.Fits(part, pr.target) {
			continue
		}
		if !pr.takes(c.Produces(part)) {
			continue
		}
		est, err := c.Estimate(part, pr.target)
		if err != nil {
			continue
		}
		return c, est, true
	}
	return nil, types.ConversionEstimate{}, false
}

// takes reports whether the target takes every modality in ms.
func (pr *planner) takes(ms []types.Modality) bool {
	for _, m := range ms {
		if m == types.ModalityText {
			continue
		}
		if l, ok := pr.target.Modalities.In[m]; !ok || len(l.Media) == 0 {
			return false
		}
	}
	return true
}

// native returns why target cannot take part as it is, or "" when it can.
// lowered is true for media in a tool result that the adapter moves to a
// follow-up user message.
func (pr *planner) native(part types.Part, src types.Source, m types.Modality, inToolResult bool) (reason, cause string, lowered bool) {
	name := offeringName(pr.target)
	if pr.target.ID != "" {
		name = pr.target.ID
	}
	switch {
	case src.Unresolved != "":
		return "unavailable: " + src.Unresolved, causeUnavailable, false
	case src.Elided():
		return "no locator left (the bytes were not persisted)", causeUnavailable, false
	case src.MediaType == "":
		return "media type unknown", "", false
	}
	lim, ok := pr.target.Modalities.In[m]
	if !ok || !slices.Contains(lim.Media, src.MediaType) {
		return fmt.Sprintf("%s is not an input %s takes", src.MediaType, name), "", false
	}
	if inToolResult && pr.target.Modalities.ToolResult != nil {
		switch pr.target.Modalities.ToolResult[m] {
		case types.ToolResultInline:
		case types.ToolResultFollowUpUser:
			lowered = true
		default:
			return fmt.Sprintf("%s does not take %s inside a tool result", name, m), "", false
		}
	}
	size := src.Size
	if size == 0 {
		size = int64(len(src.Inline))
	}
	if lim.MaxBytes > 0 && size > lim.MaxBytes {
		return fmt.Sprintf("%d bytes is over the %d byte limit of %s", size, lim.MaxBytes, name), "", false
	}
	if lim.MaxCount > 0 && pr.counts[m] >= lim.MaxCount {
		return fmt.Sprintf("more than %d %s parts", lim.MaxCount, m), "", false
	}
	if why := pr.locator(src, lim); why != "" {
		return why, "", false
	}
	return "", "", lowered
}

// locator returns why no locator of src is one the target reads, or "".
func (pr *planner) locator(src types.Source, lim types.ModalityLimit) string {
	sources := lim.Sources
	if len(sources) == 0 {
		sources = []types.SourceKind{types.SourceInline, types.SourceURI}
	}
	if slices.Contains(sources, types.SourceFile) {
		if _, ok := src.VendorFile(string(pr.target.Model.Vendor), pr.target.Endpoint.Name); ok {
			return ""
		}
	}
	if src.URI != "" && slices.Contains(sources, types.SourceURI) && pr.readsURI(src.URI) {
		return ""
	}
	if len(src.Inline) > 0 && slices.Contains(sources, types.SourceInline) {
		return ""
	}
	var have []string
	if src.URI != "" {
		have = append(have, "a "+scheme(src.URI)+": URI")
	}
	if len(src.Inline) > 0 {
		have = append(have, "inline bytes")
	}
	if len(src.Files) > 0 {
		have = append(have, "another endpoint's file")
	}
	if src.Ref != "" {
		have = append(have, "an unresolved workspace reference")
	}
	return fmt.Sprintf("no locator the endpoint reads (it takes %s; the part has %s)", joinKinds(sources), strings.Join(have, ", "))
}

// readsURI reports whether the endpoint fetches uri itself: its scheme is
// one the endpoint declares, https when it declares none.
func (pr *planner) readsURI(uri string) bool {
	schemes := pr.target.Endpoint.Files.URISchemes
	if len(schemes) == 0 {
		schemes = []string{"https"}
	}
	return slices.Contains(schemes, scheme(uri))
}

func scheme(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" {
		return ""
	}
	return strings.ToLower(u.Scheme)
}

func joinKinds(ks []types.SourceKind) string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

// thinking plans a reasoning part for a target that verifies reasoning
// signatures: one signed by another provider, or unsigned, cannot be
// replayed there.
func (pr *planner) thinking(pl *Plan, t types.ThinkingPart, path types.PartPath) {
	if !verifiesThinking(pr.target) {
		return
	}
	vendor := string(pr.target.Model.Vendor)
	var reason string
	switch {
	case t.Origin != "" && t.Origin != vendor:
		reason = "reasoning signed by " + t.Origin + " cannot be verified by " + vendor
	case !t.Redacted && t.Signature == "":
		reason = "unsigned reasoning cannot be replayed to " + vendor
	default:
		return
	}
	d := Decision{part: t, ConversionDecision: types.ConversionDecision{Path: path, Kind: types.KindThinking,
		Action: types.DecisionOmitted, Reason: reason, Scope: ScopePolicy}}
	if pr.policy.Thinking == types.ThinkingAsText && !t.Redacted && t.Text != "" {
		d.Action = types.DecisionConverted
		d.Via = "thinking-text"
	}
	pl.Decisions = append(pl.Decisions, d)
}

// verifiesThinking reports whether target checks the signature of every
// reasoning part it is sent: the Anthropic Messages API does.
func verifiesThinking(o types.Offering) bool {
	return o.Endpoint.Surface == types.SurfaceAnthropicMessages ||
		(o.Endpoint.Surface == "" && o.Model.Vendor == "anthropic")
}

// decisionFor names the recorded action of a conversion.
func decisionFor(a types.ModalityAction) string {
	switch a {
	case types.ActTranscribe:
		return types.DecisionTranscribed
	case types.ActDescribe:
		return types.DecisionDescribed
	case types.ActExtract:
		return types.DecisionExtracted
	default:
		return types.DecisionConverted
	}
}

func via(c types.Converter) string { return c.Name() + "@" + c.Version() }

func addEstimate(a, b types.ConversionEstimate) types.ConversionEstimate {
	return types.ConversionEstimate{InputTokens: a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens, Cost: a.Cost + b.Cost}
}
