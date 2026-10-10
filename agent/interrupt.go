package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// WithInterruptExpiry gives every decision a run waits for a deadline of ttl
// and decides what happens when nobody answers in time. Sub-agents inherit
// it. A ttl of zero means no deadline, which is the default.
//
// With types.InterruptExpireDeny (the zero value) an expired approval is a
// refusal, never a yes. types.InterruptExpireFail stops the run with
// types.ErrInterruptExpired. types.InterruptExpireEscalate posts the decision
// once more, attributed to the caller one level up with a fresh deadline,
// and denies it when that also expires. A root run has no caller, so it
// denies at once.
func WithInterruptExpiry(ttl time.Duration, policy types.InterruptPolicy) AgentOption {
	return func(c *AgentConfig) {
		c.InterruptTTL = ttl
		c.InterruptPolicy = policy
	}
}

// decision is the outcome of one interrupt.
type decision struct {
	approved bool
	args     map[string]any
	// message is the refusal reason or, for an approval with a note, the
	// note. For a clarification it is the answer.
	message string
	// approver names who decided, as the host reported it.
	approver string
	// grant is the scope the host attached to an approval.
	grant *types.GrantRequest
	// denied is true when a person refused, as opposed to a cancellation,
	// an expiry, or a failure to ask.
	denied bool
}

// interruptRequest describes one decision a run needs from outside.
type interruptRequest struct {
	kind    types.InterruptKind
	phase   string
	call    types.ToolCallPart
	markers []types.Marker
	payload []byte
}

// awaitInterrupt posts req, sends its MarkerDelta, and waits for a reply, the
// deadline, or cancellation. A durable ApprovalRunner records the decision
// instead, and a run with no consumer fails at once. The returned bool is
// false when the call must not proceed; the decision's message is then the
// tool error to report.
func (a *Agent) awaitInterrupt(ctx context.Context, stream *EventStream, req interruptRequest) (decision, bool) {
	if runner, ok := a.cfg.StepRunner.(types.ApprovalRunner); ok {
		in := types.Interrupt{ID: req.phase + "/" + req.call.ID, Kind: req.kind, RunID: stream.runID, Path: stream.path}
		a.interruptHooks(ctx, stream, HookInterruptRaised, in, req.call, nil, false)
		d, err := runner.ResolveApproval(ctx, types.ApprovalRequest{ID: in.ID, ToolCall: req.call, Markers: req.markers})
		out, ok := decision{message: "rejected: " + d.Message, approver: d.Approver, denied: true}, false
		switch {
		case err != nil:
			stream.stopRun(err)
			out = decision{message: err.Error()}
		case d.Approved:
			out, ok = decision{approved: true, args: d.ModifiedArgs, message: d.Message, approver: d.Approver, grant: d.Grant}, true
		}
		a.interruptHooks(ctx, stream, HookInterruptResolved, in, req.call, &out, ok)
		return out, ok
	}
	if stream.nonStreaming {
		stream.stopRun(errNonStreamingApproval)
		return decision{message: errNonStreamingApproval.Error()}, false
	}

	in := stream.newInterrupt(req.kind, req.phase, req.call.ID, req.markers, req.payload, a.cfg.InterruptTTL, a.cfg.InterruptPolicy)
	d, ok := a.waitInterrupt(ctx, stream, req, in)
	a.interruptHooks(ctx, stream, HookInterruptResolved, in, req.call, &d, ok)
	return d, ok
}

// waitInterrupt posts in and waits for its reply, applying the expiry
// policy. InterruptRaised hooks see each posting.
func (a *Agent) waitInterrupt(ctx context.Context, stream *EventStream, req interruptRequest, in types.Interrupt) (decision, bool) {
	escalated := false
	for {
		reply, err := stream.postInterrupt(in, req.call.ID)
		if err != nil {
			return decision{message: "interrupt not posted: " + err.Error()}, false
		}
		posted := in
		stream.send(types.MarkerDelta{
			ToolCallID: req.call.ID,
			ToolName:   req.call.Name,
			Arguments:  req.call.Arguments,
			Markers:    req.markers,
			Interrupt:  &posted,
		})
		a.interruptHooks(ctx, stream, HookInterruptRaised, posted, req.call, nil, false)
		select {
		case r, ok := <-reply:
			stream.withdrawInterrupt(req.call.ID)
			if ok {
				return replyDecision(r)
			}
		case <-ctx.Done():
			stream.withdrawInterrupt(req.call.ID)
			return decision{message: "context cancelled"}, false
		}

		// The interrupt expired unanswered.
		switch in.Policy.OnExpire {
		case types.InterruptExpireFail:
			err := fmt.Errorf("%w: %s", types.ErrInterruptExpired, in.ID)
			stream.stopRun(err)
			return decision{message: err.Error()}, false
		case types.InterruptExpireEscalate:
			if !escalated && len(in.Path) > 0 {
				escalated = true
				in = escalate(in, a.cfg.InterruptTTL)
				continue
			}
		}
		return decision{message: "rejected: no decision before the interrupt expired"}, false
	}
}

// escalate re-addresses an expired interrupt to the caller one level up. It
// keeps the kind and payload, takes a fresh deadline, and denies when that
// one also expires.
func escalate(in types.Interrupt, ttl time.Duration) types.Interrupt {
	out := in
	out.Path = append([]string(nil), in.Path[:len(in.Path)-1]...)
	out.ID = types.InterruptID(in.RunID, out.Path, "escalated", in.ID)
	out.CreatedAt = time.Now().UTC()
	out.ExpiresAt = time.Time{}
	if ttl > 0 {
		out.ExpiresAt = out.CreatedAt.Add(ttl)
	}
	out.Policy = types.InterruptPolicy{OnExpire: types.InterruptExpireDeny}
	return out
}

// replyDecision converts a reply. An answer counts as approval, so a
// clarification can be answered with Answer alone.
func replyDecision(r types.InterruptReply) (decision, bool) {
	answer := replyAnswer(r)
	if !r.Decision.Approved && answer == "" {
		msg := "rejected"
		if r.Decision.Message != "" {
			msg = "rejected: " + r.Decision.Message
		}
		return decision{message: msg, approver: r.Decision.Approver, denied: true}, false
	}
	if answer == "" {
		answer = r.Decision.Message
	}
	return decision{approved: true, args: r.Decision.ModifiedArgs, message: answer, approver: r.Decision.Approver, grant: r.Decision.Grant}, true
}

// replyAnswer returns a reply's Answer as text: a JSON string is unquoted and
// any other JSON value is returned as written.
func replyAnswer(r types.InterruptReply) string {
	if len(r.Answer) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Answer, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(r.Answer))
}

// interruptKind classifies an approval by what it is about.
func interruptKind(call types.ToolCallPart, phase string) types.InterruptKind {
	if call.Name == budgetToolName || strings.HasPrefix(phase, budgetToolName) {
		return types.InterruptBudget
	}
	return types.InterruptApproval
}

// forwardChildMarker re-emits a child's MarkerDelta on the parent stream
// under parentCallID/childCallID and delivers the decision to the child. The
// forwarded copy keeps the child's interrupt ID and deadline, so the root
// stream lists it and it lapses when the child's does.
//
// It returns once the marker is posted and sent; the wait for the decision
// runs on its own goroutine, tracked by pending, so the caller keeps reading
// the child's deltas. A child that asks for several decisions at once, such
// as parallel gated tools, therefore has all of them listed at the root
// while they are pending. The caller joins pending before it finishes the
// delegation. clock, when set, pauses while any decision is pending.
func (a *Agent) forwardChildMarker(ctx context.Context, stream *EventStream, parentCallID string, child *EventStream, marker types.MarkerDelta, clock *pausableDeadline, pending *sync.WaitGroup) {
	originalID := marker.ToolCallID
	marker.ToolCallID = parentCallID + "/" + originalID
	var in types.Interrupt
	if marker.Interrupt != nil {
		in = *marker.Interrupt
	} else {
		// A custom invoker's stream may not post interrupts.
		in = stream.newInterrupt(types.InterruptApproval, "subagent", marker.ToolCallID, marker.Markers, nil, 0, types.InterruptPolicy{})
	}
	reply, err := stream.postInterrupt(in, marker.ToolCallID)
	if err != nil {
		a.cfg.Logger.Warn("sub-agent marker not forwarded", "agent", a.cfg.Name, "tool_call_id", marker.ToolCallID, "error", err)
		return
	}
	marker.Interrupt = &in
	clock.pause()
	stream.send(marker)
	pending.Add(1)
	go func() {
		defer pending.Done()
		defer stream.withdrawInterrupt(marker.ToolCallID)
		defer clock.resume()
		select {
		case r, ok := <-reply:
			if !ok {
				return // expired; the child applies its own policy
			}
			if err := child.replyMarker(originalID, r); err != nil {
				a.cfg.Logger.Warn("sub-agent marker resolution not delivered", "agent", a.cfg.Name, "tool_call_id", marker.ToolCallID, "error", err)
			}
		case <-child.ctx.Done():
			// The child was cancelled, by cancel_subagent or by the run
			// ending, and no longer waits for this decision.
		case <-ctx.Done():
			child.Cancel()
		}
	}()
}
