package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// ApprovalPolicy makes approvals adapt to the decisions people already made
// in a conversation. Gates and markers still decide which calls need a
// decision. The policy decides whether a person must be asked again:
//
//   - Grants. An approval can carry a types.GrantRequest (Resolution.Grant,
//     types.ApprovalDecision.Grant): once, the tool, matching arguments, or
//     the whole conversation, with an optional expiry. Later calls the grant
//     covers are approved without asking.
//   - Repeated denials. After DenyAfter denials of one tool, later calls to it
//     are refused without asking, or the tool is hidden from the model.
//   - Risk defaults. With RiskDefaults, calls are also gated by capability
//     class: reads run, writes and undeclared tools ask, destructive tools
//     always ask.
//   - Ramp. With RampAfter, a write-class tool that people approved that many
//     times is approved without asking.
//
// Grants and the ramp never cover a destructive tool: every destructive call
// is asked about. Only the host creates grants, from the decisions it
// delivers. Nothing the model writes, and no skill or tool output, can.
//
// Every decision is recorded in the tree as types.ApprovalPart next to
// the call's result, and the policy's state is rebuilt from those records at
// the start of each run. Under a durable runner, the verdict on each call is
// a recorded step, so a replay decides the same way even after a grant
// expired. State is per conversation: a sub-agent starts with none.
type ApprovalPolicy struct {
	// RiskDefaults adds a capability gate: read runs, write and unknown ask,
	// destructive always asks. It composes with the agent's ToolGate, the
	// most restrictive verdict winning.
	RiskDefaults bool
	// DenyAfter is how many denials of one tool end further requests for it.
	// 0 never stops asking.
	DenyAfter int
	// HideDenied hides a tool that reached DenyAfter from the model instead
	// of refusing its calls with the reason.
	HideDenied bool
	// RampAfter approves a write-class tool without asking once people have
	// approved it this many times in the conversation. 0 disables the ramp.
	RampAfter int
	// Now overrides the clock that grant expiry is judged by.
	Now func() time.Time
}

// WithApprovalPolicy sets the agent's approval policy. Sub-agents inherit it.
func WithApprovalPolicy(p ApprovalPolicy) AgentOption {
	return func(c *AgentConfig) { c.ApprovalPolicy = &p }
}

func (p *ApprovalPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// riskGate is the capability gate RiskDefaults adds.
var riskGate = types.CapabilityGate(types.CapabilityPolicy{
	types.ToolCapabilityRead:        types.GateAllow,
	types.ToolCapabilityWrite:       types.GateRequireApproval,
	types.ToolCapabilityDestructive: types.GateRequireApproval,
	types.ToolCapabilityUnknown:     types.GateRequireApproval,
})

// approvalState is one conversation's grants and counts. It is rebuilt from
// the branch at the start of a run and updated as decisions are made.
type approvalState struct {
	mu        sync.Mutex
	grants    []types.Grant
	approvals map[string]int
	denials   map[string]int
}

// newApprovalState folds the approval records on a branch. Records are read
// only from system messages, which only the loop writes.
func newApprovalState(msgs []types.Message) *approvalState {
	s := &approvalState{approvals: map[string]int{}, denials: map[string]int{}}
	for _, m := range msgs {
		sm, ok := m.(types.SystemMessage)
		if !ok {
			continue
		}
		for _, c := range sm.Parts {
			if rec, ok := c.(types.ApprovalPart); ok {
				s.applyLocked(rec)
			}
		}
	}
	return s
}

func (s *approvalState) apply(rec types.ApprovalPart) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(rec)
}

func (s *approvalState) applyLocked(rec types.ApprovalPart) {
	switch rec.Event {
	case types.ApprovalEventGranted:
		if rec.Grant != nil {
			s.grants = append(s.grants, *rec.Grant)
		}
		s.approvals[rec.Tool]++
	case types.ApprovalEventApproved:
		s.approvals[rec.Tool]++
	case types.ApprovalEventDenied:
		s.denials[rec.Tool]++
	case types.ApprovalEventSnapshot:
		s.grants = slices.Clone(rec.Grants)
		s.approvals = maps.Clone(rec.Approvals)
		s.denials = maps.Clone(rec.Denials)
		if s.approvals == nil {
			s.approvals = map[string]int{}
		}
		if s.denials == nil {
			s.denials = map[string]int{}
		}
	}
}

// snapshot returns the whole state as one record, or false when there is
// nothing to carry.
func (s *approvalState) snapshot() (types.ApprovalPart, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.grants) == 0 && len(s.approvals) == 0 && len(s.denials) == 0 {
		return types.ApprovalPart{}, false
	}
	return types.ApprovalPart{
		Event:     types.ApprovalEventSnapshot,
		Grants:    slices.Clone(s.grants),
		Approvals: maps.Clone(s.approvals),
		Denials:   maps.Clone(s.denials),
	}, true
}

// carryApprovals writes the conversation's approval state onto a branch
// compaction created. Compaction keeps only what the model sees, so the
// records on the old branch do not reach the new one; without the snapshot
// a later run there would forget its grants and denial counts.
func (a *Agent) carryApprovals(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID) error {
	if stream.approvals == nil {
		return nil
	}
	rec, ok := stream.approvals.snapshot()
	if !ok {
		return nil
	}
	return a.appendToBranch(ctx, tr, branch, types.SystemMessage{Parts: []types.SystemPart{rec}})
}

// grantsSnapshot returns the recorded grants, oldest first.
func (s *approvalState) grantsSnapshot() []types.Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.grants)
}

// denied reports whether tool reached the denial limit.
func (s *approvalState) denied(tool string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return limit > 0 && s.denials[tool] >= limit
}

// verdict decides a call before anyone is asked.
func (s *approvalState) verdict(p *ApprovalPolicy, def types.ToolDef, args map[string]any, now time.Time) types.ApprovalVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.DenyAfter > 0 && s.denials[def.Name] >= p.DenyAfter {
		return types.ApprovalVerdict{Outcome: types.VerdictDeny,
			Reason: fmt.Sprintf("tool %s was denied %d times in this conversation", def.Name, s.denials[def.Name])}
	}
	class := def.Capability.Effective()
	if class == types.ToolCapabilityDestructive {
		return types.ApprovalVerdict{Outcome: types.VerdictAsk}
	}
	for i := len(s.grants) - 1; i >= 0; i-- {
		if g := s.grants[i]; g.Covers(def.Name, args, now) {
			return types.ApprovalVerdict{Outcome: types.VerdictApprove, Grant: g.ID}
		}
	}
	if p.RampAfter > 0 && class == types.ToolCapabilityWrite && s.approvals[def.Name] >= p.RampAfter {
		return types.ApprovalVerdict{Outcome: types.VerdictApprove,
			Reason: fmt.Sprintf("approved %d times in this conversation", s.approvals[def.Name])}
	}
	return types.ApprovalVerdict{Outcome: types.VerdictAsk}
}

// callApproval collects what the approval steps learn about one call.
type callApproval struct {
	approval types.CallApproval
	records  []types.ApprovalPart
}

// policyVerdict decides a call under the policy. Under a durable runner the
// verdict is a recorded step, so a replay returns it unchanged.
func (a *Agent) policyVerdict(ctx context.Context, st *approvalState, tc types.ToolCallPart, def types.ToolDef, phase string) (types.ApprovalVerdict, error) {
	decide := func() types.ApprovalVerdict {
		return st.verdict(a.cfg.ApprovalPolicy, def, tc.Arguments, a.cfg.ApprovalPolicy.now())
	}
	if _, inline := a.cfg.StepRunner.(types.NoopStepRunner); inline {
		return decide(), nil
	}
	sr, err := a.cfg.StepRunner.RunStep(ctx, "approval-"+phase+"-"+tc.ID, func(context.Context) (types.StepResult, error) {
		v := decide()
		return types.StepResult{Kind: types.StepKindApproval, ToolCallID: tc.ID, Approval: &v}, nil
	})
	if err != nil {
		return types.ApprovalVerdict{}, err
	}
	if sr.Approval == nil {
		return types.ApprovalVerdict{Outcome: types.VerdictAsk}, nil
	}
	return *sr.Approval, nil
}

// requestApproval obtains a decision on a call that a gate or a marker
// held. Without a policy it asks, as before. With one, a grant or the ramp
// may approve it and the denial limit may refuse it without asking, and
// each decision is recorded on ca.
func (a *Agent) requestApproval(ctx context.Context, stream *EventStream, tc types.ToolCallPart, def types.ToolDef, markers []types.Marker, phase string, ca *callApproval) (decision, bool) {
	st := stream.approvals
	if a.cfg.ApprovalPolicy == nil || st == nil {
		d, ok := a.awaitApprovalPhase(ctx, stream, tc, markers, phase)
		if ok {
			ca.approval = types.CallApproval{Required: true, Approver: d.approver}
		}
		return d, ok
	}
	v, err := a.policyVerdict(ctx, st, tc, def, phase)
	if err != nil {
		stream.stopRun(err)
		return decision{message: err.Error()}, false
	}
	record := func(rec types.ApprovalPart) {
		rec.Tool, rec.ToolCallID = def.Name, tc.ID
		st.apply(rec)
		ca.records = append(ca.records, rec)
	}
	switch v.Outcome {
	case types.VerdictDeny:
		record(types.ApprovalPart{Event: types.ApprovalEventAutoDenied, Reason: v.Reason})
		return decision{message: "refused: " + v.Reason}, false
	case types.VerdictApprove:
		record(types.ApprovalPart{Event: types.ApprovalEventAutoApproved, GrantID: v.Grant, Reason: v.Reason})
		ca.approval = types.CallApproval{Required: true, Grant: v.Grant}
		return decision{approved: true}, true
	}

	d, ok := a.awaitApprovalPhase(ctx, stream, tc, markers, phase)
	switch {
	case ok:
		ca.approval = types.CallApproval{Required: true, Approver: d.approver}
		rec := types.ApprovalPart{Event: types.ApprovalEventApproved, Approver: d.approver}
		if g, granted := a.grantFrom(d, def, tc.ID); granted {
			rec.Event, rec.Grant = types.ApprovalEventGranted, &g
		}
		record(rec)
	case d.denied:
		record(types.ApprovalPart{Event: types.ApprovalEventDenied, Approver: d.approver, Reason: d.message})
	}
	return d, ok
}

// grantFrom turns the scope a person attached to an approval into a grant.
// A destructive tool is never granted beyond the call, and an invalid
// request approves the call alone.
func (a *Agent) grantFrom(d decision, def types.ToolDef, callID string) (types.Grant, bool) {
	if d.grant == nil || def.Capability.Effective() == types.ToolCapabilityDestructive {
		return types.Grant{}, false
	}
	if err := d.grant.Validate(); err != nil {
		a.cfg.Logger.Warn("approval grant ignored", "agent", a.cfg.Name, "tool", def.Name, "error", err)
		return types.Grant{}, false
	}
	return types.NewGrant(*d.grant, def.Name, callID, d.approver)
}

// hideDenied removes tools that reached the denial limit from a turn, when
// the policy hides them rather than refusing their calls.
func (a *Agent) hideDenied(stream *EventStream, active activeContext) activeContext {
	p, st := a.cfg.ApprovalPolicy, stream.approvals
	if p == nil || st == nil || !p.HideDenied || p.DenyAfter <= 0 {
		return active
	}
	kept := types.NewToolRegistry()
	hidden := false
	for _, t := range active.tools.All() {
		if st.denied(t.Definition().Name, p.DenyAfter) {
			hidden = true
			continue
		}
		kept.Register(t)
	}
	if !hidden {
		return active
	}
	active.tools, active.toolDefs = kept, kept.Definitions()
	return active
}

// Grants returns the grants recorded on a branch, oldest first, including
// expired ones.
func Grants(msgs []types.Message) []types.Grant {
	return newApprovalState(msgs).grantsSnapshot()
}
