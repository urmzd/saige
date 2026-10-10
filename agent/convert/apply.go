package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// Priced is implemented by a converter that calls a model, so its call is
// reserved against the attempt's budget before it runs. A converter that
// calls no model, such as an extractor, is free and is not reserved.
type Priced interface {
	Pricing() types.Pricing
}

// StepPrefix starts the durable step name of every conversion:
// "convert:<digest>:<converter>@<version>". The name does not depend on the
// turn, so a conversion recorded once is replayed, never repeated, however
// often the history that holds the part is sent again.
const StepPrefix = "convert:"

// Apply runs the plan on msgs and returns the converted copy, the executed
// report and an error when a planned conversion failed and no later
// permitted action could serve the part. msgs is never modified: the
// conversation keeps its original parts, and only the view sent to the
// provider is converted.
//
// A lowered part, media in a tool result the target takes only from the
// user, is replaced in the tool result by a notice and moved to a user
// message placed right after the tool results of its turn (see lower).
//
// Each conversion is looked up in cache by the policy's scope, the
// converter's Name@Version and the part's digest (or URI); a miss runs as
// a durable step under rt.Steps and, for a priced converter, is carved from
// rt.Reservation and settled on rt.Budget.
func (p Plan) Apply(ctx context.Context, msgs []types.Message, rt Runtime, cache types.ConversionCache) ([]types.Message, types.ConversionReport, error) {
	rep := types.ConversionReport{Offering: p.Offering}
	subst := map[types.PartPath][]types.Part{}
	eg, _ := types.EgressFrom(ctx)
	ex := executor{plan: p, rt: rt, cache: cache, egress: eg}
	var lowered []Decision
	for _, d := range p.Decisions {
		dec := d.ConversionDecision
		switch dec.Action {
		case types.DecisionNative:
		case types.DecisionLowered:
			subst[dec.Path] = []types.Part{types.TextPart{Text: fmt.Sprintf("[%s attached in the next message]", describePart(d.part))}}
			lowered = append(lowered, d)
		case types.DecisionRejected:
			return nil, rep, &RejectError{Offering: p.Offering, Rejected: []types.ConversionDecision{dec}}
		case types.DecisionOmitted:
			subst[dec.Path] = omitted(d)
		default:
			if t, ok := d.part.(types.ThinkingPart); ok {
				subst[dec.Path] = []types.Part{types.TextPart{Text: "[reasoning] " + t.Text}}
				break
			}
			parts, done, err := ex.run(ctx, d)
			if err != nil {
				return nil, rep, err
			}
			dec = done
			subst[dec.Path] = parts
		}
		rep.Decisions = append(rep.Decisions, dec)
	}
	rep.Hash = rep.ComputeHash()
	if err := tokenizeSubst(ctx, eg.Vault, subst); err != nil {
		return nil, rep, err
	}
	return lower(substitute(msgs, subst), followUps(msgs, lowered)), rep, nil
}

// followUps groups the lowered parts by the message holding their tool
// result, in request order, each tool result's media under a label naming
// its call.
func followUps(msgs []types.Message, lowered []Decision) map[int][]types.UserPart {
	if len(lowered) == 0 {
		return nil
	}
	names := map[string]string{}
	for _, m := range msgs {
		if am, ok := m.(types.AssistantMessage); ok {
			for _, p := range am.Parts {
				if c, ok := p.(types.ToolCallPart); ok {
					names[c.ID] = c.Name
				}
			}
		}
	}
	out := map[int][]types.UserPart{}
	last := types.PartPath{Message: -1}
	for _, d := range lowered {
		up, ok := d.part.(types.UserPart)
		if !ok {
			continue
		}
		mi := d.Path.Message
		if mi != last.Message || d.Path.Part != last.Part {
			id := ""
			if tr, ok := types.PartsOf(msgs[mi])[d.Path.Part].(types.ToolResultPart); ok {
				id = tr.CallID
			}
			label := "[output of tool call " + id
			if n := names[id]; n != "" {
				label += " (" + n + ")"
			}
			out[mi] = append(out[mi], types.TextPart{Text: label + "]"})
		}
		out[mi] = append(out[mi], up)
		last = d.Path
	}
	return out
}

// lower places the lowered media of each turn in one user message right
// after that turn's tool results. The tool results of a turn are the run of
// consecutive messages that hold them; they stay contiguous, so they still
// answer the assistant's calls directly, and a user message after them is
// valid on every surface. A user message that holds tool results and its
// own content is split, the follow-up between the two, as the adapters send
// tool results before the user's content.
func lower(msgs []types.Message, follow map[int][]types.UserPart) []types.Message {
	if len(follow) == 0 {
		return msgs
	}
	out := make([]types.Message, 0, len(msgs)+len(follow))
	var pending []types.UserPart
	flush := func() {
		if len(pending) > 0 {
			out = append(out, types.UserMessage{Parts: pending})
			pending = nil
		}
	}
	for mi, msg := range msgs {
		pending = append(pending, follow[mi]...)
		if um, ok := msg.(types.UserMessage); ok && len(pending) > 0 && holdsToolResult(um) {
			var results, rest []types.UserPart
			own := false
			for _, p := range um.Parts {
				if _, ok := p.(types.ToolResultPart); ok {
					results = append(results, p)
					continue
				}
				rest = append(rest, p)
				own = own || !types.IsMetadata(p)
			}
			if own {
				out = append(out, types.UserMessage{Parts: results})
				flush()
				out = append(out, types.UserMessage{Parts: rest})
				continue
			}
		}
		out = append(out, msg)
		if mi+1 == len(msgs) || !holdsToolResult(msgs[mi+1]) {
			flush()
		}
	}
	return out
}

// holdsToolResult reports whether m carries a tool result.
func holdsToolResult(m types.Message) bool {
	for _, p := range types.PartsOf(m) {
		if _, ok := p.(types.ToolResultPart); ok {
			return true
		}
	}
	return false
}

// tokenizeSubst tokenizes the text of every replacement with the privacy
// boundary's vault, so a transcript, a description, an extracted text or a
// notice naming a file never reaches the provider with the values the
// boundary keeps from it. Converted parts are cached and journaled as the
// converter made them; they are tokenized here, after the cache, so a cache
// shared between sessions never holds one session's placeholders.
func tokenizeSubst(ctx context.Context, v types.PlaceholderVault, subst map[types.PartPath][]types.Part) error {
	if v == nil {
		return nil
	}
	for path, parts := range subst {
		for i, part := range parts {
			t, ok := part.(types.TextPart)
			if !ok {
				continue
			}
			text, err := v.Tokenize(ctx, t.Text)
			if err != nil {
				// Fail closed: a view that could not be tokenized is not sent.
				return fmt.Errorf("conversion: tokenize output of part %s: %w", path, err)
			}
			parts[i] = types.TextPart{Text: text}
		}
	}
	return nil
}

// omitted is what stands in for an omitted part: a notice for media, so the
// model knows something was left out, and nothing for reasoning.
func omitted(d Decision) []types.Part {
	if d.Kind == types.KindThinking {
		return nil
	}
	return []types.Part{types.TextPart{Text: fmt.Sprintf("[%s omitted: %s]", describePart(d.part), d.Reason)}}
}

// describePart names a media part for a notice: its file name, URI or media
// type.
func describePart(p types.Part) string {
	src, _ := types.SourceOf(p)
	switch {
	case src.Filename != "":
		return string(p.Kind()) + " " + src.Filename
	case src.URI != "":
		return string(p.Kind()) + " " + src.URI
	case src.MediaType != "":
		return string(p.Kind()) + " " + string(src.MediaType)
	}
	return string(p.Kind())
}

type executor struct {
	plan   Plan
	rt     Runtime
	cache  types.ConversionCache
	egress types.Egress
}

// EgressReporter is implemented by a converter that sends the part to a
// model: it reports the offering the part goes to, so a privacy boundary
// that requires text can refuse an endpoint not cleared for personal data.
type EgressReporter interface {
	EgressOffering() (types.Offering, bool)
}

// egressAllowed reports why c may not receive a part under the boundary,
// or "" when it may. Only a boundary that requires text restricts
// converters: under any other policy the media reaches a vendor anyway.
func (ex executor) egressAllowed(c types.Converter) string {
	if !ex.egress.RequireText {
		return ""
	}
	if _, priced := c.(Priced); !priced {
		return ""
	}
	r, ok := c.(EgressReporter)
	if !ok {
		return "converter sends media to a model whose endpoint is unknown"
	}
	off, ok := r.EgressOffering()
	if !ok || !off.Endpoint.Data.PIIOK {
		return "converter endpoint is not cleared for personal data (data.pii_ok)"
	}
	return ""
}

// run converts one part, falling back to the actions permitted after the
// planned one when its converter fails.
func (ex executor) run(ctx context.Context, d Decision) ([]types.Part, types.ConversionDecision, error) {
	dec := d.ConversionDecision
	c, actions := d.converter, d.rest
	var failures []string
	for {
		parts, usage, err := ex.convert(ctx, c, d)
		if err == nil {
			dec.Action, dec.Via = decisionFor(c.Action()), via(c)
			dec.Cached = usage.Cached
			if usage.Cost != 0 {
				cost := usage.Cost
				dec.Cost = &cost
			}
			dec.Produced = produced(parts)
			if len(failures) > 0 {
				dec.Reason += "; " + joinFailures(failures)
			}
			return parts, dec, nil
		}
		if ctx.Err() != nil || errors.Is(err, types.ErrBudgetAdmission) {
			return nil, dec, err
		}
		failures = append(failures, via(c)+": "+err.Error())
		next, rest, omit := ex.next(d.part, actions)
		switch {
		case omit:
			dec.Action, dec.Via = types.DecisionOmitted, ""
			dec.Reason += "; " + joinFailures(failures)
			return omitted(Decision{part: d.part, ConversionDecision: dec}), dec, nil
		case next == nil:
			dec.Action, dec.Via = types.DecisionRejected, ""
			dec.Reason += "; " + joinFailures(failures)
			return nil, dec, &RejectError{Offering: ex.plan.Offering, Rejected: []types.ConversionDecision{dec}}
		}
		c, actions = next, rest
	}
}

// next finds the converter for the first remaining action that has one.
// omit is true when the first such action is omit.
func (ex executor) next(part types.Part, actions []types.ModalityAction) (types.Converter, []types.ModalityAction, bool) {
	pr := planner{target: ex.plan.target, policy: ex.plan.policy}
	for i, a := range actions {
		switch a {
		case types.ActReject:
			return nil, nil, false
		case types.ActOmit:
			return nil, nil, true
		}
		if c, _, ok := pr.converter(a, part); ok {
			return c, slices.Clone(actions[i+1:]), false
		}
	}
	return nil, nil, false
}

func joinFailures(fs []string) string {
	out := "failed: " + fs[0]
	for _, f := range fs[1:] {
		out += "; " + f
	}
	return out
}

// convert runs c on the part of d, from the cache when it can.
func (ex executor) convert(ctx context.Context, c types.Converter, d Decision) ([]types.Part, types.ConversionUsage, error) {
	if reason := ex.egressAllowed(c); reason != "" {
		return nil, types.ConversionUsage{}, errors.New(reason)
	}
	id := identity(d.part)
	key := ex.plan.policy.Scope + "\x00" + via(c) + "\x00" + id
	memo := ex.cache != nil && memoizable(d.part, ex.plan.policy.Scope)
	if memo {
		if e, ok := ex.cache.Get(ctx, key); ok {
			return e.Parts, types.ConversionUsage{Cached: true}, nil
		}
	}
	est, _ := c.Estimate(d.part, ex.plan.target)
	env := types.ConvertEnv{Target: ex.plan.target, Path: d.Path, Scope: ex.plan.policy.Scope, Vault: ex.egress.Vault}
	steps := ex.rt.Steps
	if steps == nil {
		steps = types.NoopStepRunner{}
	}
	name := StepPrefix + id + ":" + via(c)
	if id == "" {
		// Media without a digest or URI has no stable name: run it inline.
		steps = types.NoopStepRunner{}
	}
	priced, isPriced := c.(Priced)
	stepCtx := ctx
	if !isPriced {
		// A converter that calls no model can safely run again when a crash
		// left its outcome unknown.
		stepCtx = types.WithIdempotentStep(ctx)
	}
	ran := false
	var usage types.ConversionUsage
	res, err := steps.RunStep(stepCtx, name, func(stepCtx context.Context) (types.StepResult, error) {
		ran = true
		out := types.StepResult{Kind: types.StepKindConvert}
		var resv types.BudgetReservation
		charged := false
		if b := ex.rt.Budget; b != nil && isPriced {
			r, err := b.Carve(ex.rt.Reservation, types.NewID(), priced.Pricing(), est.Cost, est.InputTokens+est.OutputTokens)
			if err != nil {
				return out, fmt.Errorf("%w: conversion %s: %w", types.ErrBudgetAdmission, via(c), err)
			}
			resv, charged = r, true
		}
		parts, u, cerr := c.Convert(converterContext(stepCtx), d.part, env)
		usage = u
		if charged {
			pricing := u.Pricing
			if pricing.IsZero() {
				pricing = priced.Pricing()
			}
			model := u.Model
			if model == "" {
				model = via(c)
			}
			unknown := cerr != nil && u.Usage.PromptTokens == 0 && u.Usage.CompletionTokens == 0
			// An overshoot of the reservation is recorded honestly by Settle.
			_ = ex.rt.Budget.Settle(resv.ID, model, pricing, types.UsageFromDelta(u.Usage), unknown)
			receipt := ex.rt.Budget.Receipt(resv.ID)
			out.Receipt = &receipt
			usage.Cost = receipt.Cost
		}
		if cerr != nil {
			// A failed conversion is a known outcome, recorded so a replay
			// fails over the same way without calling the converter again.
			out.ToolError = cerr.Error()
			return out, nil
		}
		uu := u.Usage
		out.Usage = &uu
		out.Conversion = &types.ConversionEntry{Parts: parts, Via: via(c)}
		return out, nil
	})
	if err != nil {
		return nil, usage, err
	}
	if res.Receipt != nil {
		if !ran && ex.rt.Budget != nil {
			if rerr := ex.rt.Budget.Restore(*res.Receipt); rerr != nil {
				return nil, usage, rerr
			}
		}
		if !ran {
			usage.Cost = res.Receipt.Cost
		}
		if ex.rt.OnReceipt != nil {
			ex.rt.OnReceipt(*res.Receipt)
		}
	}
	if res.ToolError != "" {
		return nil, usage, errors.New(res.ToolError)
	}
	if res.Conversion == nil {
		return nil, usage, fmt.Errorf("conversion %s recorded no result", via(c))
	}
	if memo {
		ex.cache.Put(ctx, key, *res.Conversion)
	}
	return types.CloneParts(res.Conversion.Parts), usage, nil
}

// identity is what a conversion of p is memoized by: its digest, else its
// URI. Empty means it cannot be memoized.
func identity(p types.Part) string {
	src, _ := types.SourceOf(p)
	switch {
	case src.Digest != "":
		return src.Digest
	case src.URI != "":
		return "uri:" + src.URI
	}
	return ""
}

// memoizable reports whether a conversion of p may be memoized. A digest
// is a content address: whoever holds the same bytes gets the same output,
// so it is memoized with or without a scope. A URI is a name another tenant
// may resolve to other content, or to content only the first could read,
// so it is memoized only under a named scope (ConversionPolicy.Scope).
func memoizable(p types.Part, scope string) bool {
	src, _ := types.SourceOf(p)
	return src.Digest != "" || (src.URI != "" && scope != "")
}

// produced references the derivative parts: a media part by its digest or
// reference, text by a digest of the text.
func produced(parts []types.Part) []string {
	var out []string
	for _, p := range parts {
		if src, ok := types.SourceOf(p); ok {
			switch {
			case src.Digest != "":
				out = append(out, string(p.Kind())+":sha256:"+src.Digest)
			case src.Ref != "":
				out = append(out, src.Ref)
			default:
				out = append(out, string(p.Kind()))
			}
			continue
		}
		if t, ok := p.(types.TextPart); ok {
			sum := sha256.Sum256([]byte(t.Text))
			out = append(out, "text:sha256:"+hex.EncodeToString(sum[:8]))
			continue
		}
		out = append(out, string(p.Kind()))
	}
	return out
}

// substitute returns msgs with the parts at each path replaced. A message
// with no replacement is reused as is.
func substitute(msgs []types.Message, subst map[types.PartPath][]types.Part) []types.Message {
	if len(subst) == 0 {
		return msgs
	}
	byMsg := map[int]bool{}
	for path := range subst {
		byMsg[path.Message] = true
	}
	out := make([]types.Message, len(msgs))
	for mi, msg := range msgs {
		if !byMsg[mi] {
			out[mi] = msg
			continue
		}
		switch v := msg.(type) {
		case types.UserMessage:
			out[mi] = types.UserMessage{Parts: replaceParts[types.UserPart](mi, v.Parts, subst)}
		case types.SystemMessage:
			out[mi] = types.SystemMessage{Parts: replaceParts[types.SystemPart](mi, v.Parts, subst)}
		case types.AssistantMessage:
			out[mi] = types.AssistantMessage{Parts: replaceParts[types.AssistantPart](mi, v.Parts, subst)}
		default:
			out[mi] = msg
		}
	}
	return out
}

// replaceParts rebuilds one message's parts. Replacements that do not fit
// the role are left out; converters produce text or media, which every role
// that holds media takes.
func replaceParts[P types.Part](mi int, parts []P, subst map[types.PartPath][]types.Part) []P {
	out := make([]P, 0, len(parts))
	for pi, part := range parts {
		path := types.PartPath{Message: mi, Part: pi, Nested: -1}
		if repl, ok := subst[path]; ok {
			for _, r := range repl {
				if rp, ok := r.(P); ok {
					out = append(out, rp)
				}
			}
			continue
		}
		if tr, ok := any(part).(types.ToolResultPart); ok {
			if np, changed := replaceNested(mi, pi, tr, subst); changed {
				if rp, ok := any(np).(P); ok {
					out = append(out, rp)
					continue
				}
			}
		}
		out = append(out, part)
	}
	return out
}

func replaceNested(mi, pi int, tr types.ToolResultPart, subst map[types.PartPath][]types.Part) (types.ToolResultPart, bool) {
	changed := false
	nested := make([]types.ToolOutputPart, 0, len(tr.Parts))
	for ni, np := range tr.Parts {
		repl, ok := subst[types.PartPath{Message: mi, Part: pi, Nested: ni}]
		if !ok {
			nested = append(nested, np)
			continue
		}
		changed = true
		for _, r := range repl {
			if op, ok := r.(types.ToolOutputPart); ok {
				nested = append(nested, op)
			}
		}
	}
	if !changed {
		return tr, false
	}
	out := tr
	out.Parts = nested
	return out, true
}
